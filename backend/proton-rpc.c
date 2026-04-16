#include "proton-rpc.h"

#include <json-glib/json-glib.h>
#include <gio/gunixsocketaddress.h>

struct _ProtonRpc {
  GSocketConnection *conn;
  GDataInputStream  *reader;
  GOutputStream     *writer;
  GMutex             lock;
  guint64            next_id;
};

/* ---------- helpers ---------- */

static JsonNode *
build_request (guint64 id, const gchar *method, JsonBuilder *params_builder)
{
  g_autoptr(JsonBuilder) b = json_builder_new ();

  json_builder_begin_object (b);
  json_builder_set_member_name (b, "id");
  json_builder_add_int_value (b, (gint64) id);
  json_builder_set_member_name (b, "method");
  json_builder_add_string_value (b, method);
  if (params_builder)
    {
      json_builder_set_member_name (b, "params");
      json_builder_add_value (b, json_builder_get_root (params_builder));
    }
  json_builder_end_object (b);

  return json_builder_get_root (b);
}

static gboolean
send_request (ProtonRpc *rpc, JsonNode *req, GError **error)
{
  g_autoptr(JsonGenerator) gen = json_generator_new ();
  json_generator_set_root (gen, req);
  gsize len;
  gchar *line = json_generator_to_data (gen, &len);

  gboolean ok = (g_output_stream_write_all (rpc->writer, line, len, NULL, NULL, error) &&
                 g_output_stream_write_all (rpc->writer, "\n", 1, NULL, NULL, error));
  g_free (line);
  return ok;
}

/* Returns a new reference to the parsed response object, or NULL on error. */
static JsonObject *
recv_response (ProtonRpc *rpc, G_GNUC_UNUSED guint64 expected_id, GError **error)
{
  gsize len;
  gchar *line = g_data_input_stream_read_line (rpc->reader, &len, NULL, error);
  if (!line)
    return NULL;

  g_autoptr(JsonParser) parser = json_parser_new ();
  if (!json_parser_load_from_data (parser, line, (gssize) len, error))
    {
      g_free (line);
      return NULL;
    }
  g_free (line);

  JsonObject *obj = json_node_get_object (json_parser_get_root (parser));

  if (json_object_has_member (obj, "error"))
    {
      JsonObject *err = json_object_get_object_member (obj, "error");
      g_set_error (error, G_IO_ERROR, G_IO_ERROR_FAILED, "%s",
                   json_object_get_string_member (err, "message"));
      return NULL;
    }

  return json_object_ref (obj);
}

static JsonObject *
call (ProtonRpc    *rpc,
      const gchar  *method,
      JsonBuilder  *params,
      GError      **error)
{
  g_mutex_lock (&rpc->lock);
  guint64 id = rpc->next_id++;

  g_autoptr(JsonNode) req = build_request (id, method, params);
  if (!send_request (rpc, req, error))
    {
      g_mutex_unlock (&rpc->lock);
      return NULL;
    }

  JsonObject *resp = recv_response (rpc, id, error);
  g_mutex_unlock (&rpc->lock);
  return resp;
}

/* ---------- public API ---------- */

ProtonRpc *
proton_rpc_new (const gchar *socket_path, GError **error)
{
  g_autoptr(GSocketClient) client = g_socket_client_new ();
  g_autoptr(GUnixSocketAddress) addr =
    (GUnixSocketAddress *) g_unix_socket_address_new (socket_path);

  GSocketConnection *conn = g_socket_client_connect (
    client, (GSocketConnectable *) addr, NULL, error);
  if (!conn)
    return NULL;

  ProtonRpc *rpc  = g_new0 (ProtonRpc, 1);
  rpc->conn       = conn; /* takes ownership */
  rpc->writer     = g_io_stream_get_output_stream (G_IO_STREAM (conn));
  GInputStream *raw = g_io_stream_get_input_stream (G_IO_STREAM (conn));
  rpc->reader     = g_data_input_stream_new (raw);
  g_data_input_stream_set_newline_type (rpc->reader, G_DATA_STREAM_NEWLINE_TYPE_LF);
  g_mutex_init (&rpc->lock);
  rpc->next_id    = 1;
  return rpc;
}

void
proton_rpc_free (ProtonRpc *rpc)
{
  if (!rpc)
    return;
  g_object_unref (rpc->reader);
  g_object_unref (rpc->conn);
  g_mutex_clear (&rpc->lock);
  g_free (rpc);
}

gboolean
proton_rpc_auth (ProtonRpc   *rpc,
                 const gchar *username,
                 const gchar *password,
                 GError     **error)
{
  g_autoptr(JsonBuilder) b = json_builder_new ();
  json_builder_begin_object (b);
  json_builder_set_member_name (b, "username");
  json_builder_add_string_value (b, username);
  json_builder_set_member_name (b, "password");
  json_builder_add_string_value (b, password);
  json_builder_end_object (b);

  g_autoptr(JsonObject) resp = call (rpc, "Auth", b, error);
  return resp != NULL;
}

ProtonEntry **
proton_rpc_list_dir (ProtonRpc   *rpc,
                     const gchar *path,
                     GError     **error)
{
  g_autoptr(JsonBuilder) b = json_builder_new ();
  json_builder_begin_object (b);
  json_builder_set_member_name (b, "path");
  json_builder_add_string_value (b, path);
  json_builder_end_object (b);

  g_autoptr(JsonObject) resp = call (rpc, "ListDir", b, error);
  if (!resp)
    return NULL;

  JsonObject *result = json_object_get_object_member (resp, "result");
  JsonArray  *arr    = json_object_get_array_member (result, "entries");
  guint       n      = json_array_get_length (arr);

  ProtonEntry **entries = g_new0 (ProtonEntry *, n + 1);
  for (guint i = 0; i < n; i++)
    {
      JsonObject  *e   = json_array_get_object_element (arr, i);
      ProtonEntry *ent = g_new0 (ProtonEntry, 1);
      ent->name   = g_strdup (json_object_get_string_member (e, "name"));
      ent->is_dir = json_object_get_boolean_member (e, "is_dir");
      ent->size   = json_object_get_int_member (e, "size");
      ent->mtime  = json_object_get_int_member (e, "mtime");
      entries[i]  = ent;
    }

  return entries;
}

ProtonEntry *
proton_rpc_stat (ProtonRpc   *rpc,
                 const gchar *path,
                 GError     **error)
{
  g_autoptr(JsonBuilder) b = json_builder_new ();
  json_builder_begin_object (b);
  json_builder_set_member_name (b, "path");
  json_builder_add_string_value (b, path);
  json_builder_end_object (b);

  g_autoptr(JsonObject) resp = call (rpc, "Stat", b, error);
  if (!resp)
    return NULL;

  JsonObject  *e   = json_object_get_object_member (resp, "result");
  ProtonEntry *ent = g_new0 (ProtonEntry, 1);
  ent->name   = g_strdup (json_object_get_string_member (e, "name"));
  ent->is_dir = json_object_get_boolean_member (e, "is_dir");
  ent->size   = json_object_get_int_member (e, "size");
  ent->mtime  = json_object_get_int_member (e, "mtime");
  return ent;
}

gssize
proton_rpc_read_file (ProtonRpc   *rpc,
                      const gchar *path,
                      gint64       offset,
                      gint64       length,
                      guchar      *buf,
                      gboolean    *eof,
                      GError     **error)
{
  g_autoptr(JsonBuilder) b = json_builder_new ();
  json_builder_begin_object (b);
  json_builder_set_member_name (b, "path");
  json_builder_add_string_value (b, path);
  json_builder_set_member_name (b, "offset");
  json_builder_add_int_value (b, offset);
  json_builder_set_member_name (b, "length");
  json_builder_add_int_value (b, length);
  json_builder_end_object (b);

  g_autoptr(JsonObject) resp = call (rpc, "ReadFile", b, error);
  if (!resp)
    return -1;

  JsonObject *result = json_object_get_object_member (resp, "result");
  if (eof)
    *eof = json_object_get_boolean_member (result, "eof");

  /* "data" is base64-encoded by encoding/json */
  const gchar *b64  = json_object_get_string_member (result, "data");
  gsize        dlen = 0;
  guchar      *data = g_base64_decode (b64, &dlen);

  gssize n = (gssize) MIN (dlen, (gsize) length);
  memcpy (buf, data, n);
  g_free (data);
  return n;
}

void
proton_entry_free (ProtonEntry *entry)
{
  if (!entry)
    return;
  g_free (entry->name);
  g_free (entry);
}
