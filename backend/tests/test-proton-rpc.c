#include <glib.h>
#include <glib/gstdio.h>
#include <gio/gio.h>
#include <gio/gunixsocketaddress.h>
#include <json-glib/json-glib.h>

#include "proton-rpc.h"

/* ---------- mock server ----------
 *
 * Runs in a background thread.  Accepts one connection, handles a fixed
 * sequence of requests, then exits.  Each fixture spins its own server
 * with a unique socket path so tests can run in parallel.
 */

typedef struct {
  gchar    *socket_path;
  GThread  *thread;
  /* responses to send, in order */
  const gchar **responses;
  guint         n_responses;
} MockServer;

static gpointer
mock_server_thread (gpointer data)
{
  MockServer *srv = data;

  GError  *error = NULL;
  GSocket *server = g_socket_new (G_SOCKET_FAMILY_UNIX,
                                  G_SOCKET_TYPE_STREAM,
                                  G_SOCKET_PROTOCOL_DEFAULT, &error);
  g_assert_no_error (error);

  GUnixSocketAddress *addr =
    (GUnixSocketAddress *) g_unix_socket_address_new (srv->socket_path);
  g_socket_bind (server, (GSocketAddress *) addr, TRUE, &error);
  g_assert_no_error (error);
  g_object_unref (addr);
  g_socket_listen (server, &error);
  g_assert_no_error (error);

  GSocket *raw_client = g_socket_accept (server, NULL, &error);
  GSocketConnection *client = g_socket_connection_factory_create_connection (raw_client);
  g_object_unref (raw_client);
  g_assert_no_error (error);

  GOutputStream    *out    = g_io_stream_get_output_stream (G_IO_STREAM (client));
  GInputStream     *raw_in = g_io_stream_get_input_stream (G_IO_STREAM (client));
  GDataInputStream *in     = g_data_input_stream_new (raw_in);
  g_data_input_stream_set_newline_type (in, G_DATA_STREAM_NEWLINE_TYPE_LF);

  for (guint i = 0; i < srv->n_responses; i++)
    {
      /* consume the incoming request line */
      gsize len;
      gchar *line = g_data_input_stream_read_line (in, &len, NULL, &error);
      g_assert_no_error (error);
      g_free (line);

      /* send the canned response */
      const gchar *resp = srv->responses[i];
      g_output_stream_write_all (out, resp, strlen (resp), NULL, NULL, &error);
      g_assert_no_error (error);
      g_output_stream_write_all (out, "\n", 1, NULL, NULL, &error);
      g_assert_no_error (error);
    }

  g_object_unref (in);
  g_object_unref (client);
  g_object_unref (server);
  return NULL;
}

static MockServer *
mock_server_start (const gchar **responses, guint n)
{
  MockServer *srv      = g_new0 (MockServer, 1);
  srv->socket_path     = g_strdup_printf ("/tmp/test-proton-rpc-%d.sock", g_test_rand_int ());
  srv->responses       = responses;
  srv->n_responses     = n;
  g_unlink (srv->socket_path);
  srv->thread = g_thread_new ("mock-server", mock_server_thread, srv);
  g_usleep (20000); /* give the thread time to bind */
  return srv;
}

static void
mock_server_stop (MockServer *srv)
{
  g_thread_join (srv->thread);
  g_unlink (srv->socket_path);
  g_free (srv->socket_path);
  g_free (srv);
}

/* ---------- tests ---------- */

/* "aGVsbG8=" = base64("hello") — used as a stand-in for salted_passphrase */
#define FAKE_SP_B64 "aGVsbG8="

static void
test_auth_success (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"result\":{"
    "\"uid\":\"uid-abc\","
    "\"refresh_token\":\"tok-xyz\","
    "\"salted_passphrase\":\"" FAKE_SP_B64 "\"}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error          = NULL;
  gchar     *uid            = NULL;
  gchar     *refresh_token  = NULL;
  GBytes    *salted_pass    = NULL;
  ProtonRpc *rpc            = proton_rpc_new (srv->socket_path, &error);
  g_assert_no_error (error);
  g_assert_nonnull (rpc);

  gboolean ok = proton_rpc_auth (rpc, "user@proton.me", "secret",
                                  &uid, &refresh_token, &salted_pass, &error);
  g_assert_no_error (error);
  g_assert_true (ok);
  g_assert_cmpstr (uid, ==, "uid-abc");
  g_assert_cmpstr (refresh_token, ==, "tok-xyz");
  g_assert_nonnull (salted_pass);
  gsize sp_len;
  const guchar *sp = g_bytes_get_data (salted_pass, &sp_len);
  g_assert_cmpmem (sp, sp_len, "hello", 5);

  g_free (uid);
  g_free (refresh_token);
  g_bytes_unref (salted_pass);
  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_auth_failure (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"error\":{\"code\":-32002,\"message\":\"authentication failed\"}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error = NULL;
  ProtonRpc *rpc   = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  gboolean ok = proton_rpc_auth (rpc, "user@proton.me", "wrong",
                                  NULL, NULL, NULL, &error);
  g_assert_false (ok);
  g_assert_nonnull (error);
  g_assert_cmpstr (error->message, ==, "authentication failed");
  g_error_free (error);

  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_resume_session (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"result\":{\"ok\":true}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError  *error    = NULL;
  GBytes  *sp       = g_bytes_new_static ("hello", 5);
  ProtonRpc *rpc    = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  gboolean ok = proton_rpc_resume_session (rpc, "uid-abc", "tok-xyz", sp, NULL, NULL, &error);
  g_assert_no_error (error);
  g_assert_true (ok);

  g_bytes_unref (sp);
  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_list_dir (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"result\":{\"entries\":["
    "{\"name\":\"Documents\",\"is_dir\":true,\"size\":0,\"mtime\":1700000000},"
    "{\"name\":\"photo.jpg\",\"is_dir\":false,\"size\":204800,\"mtime\":1700000001}"
    "]}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error = NULL;
  ProtonRpc *rpc   = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  ProtonEntry **entries = proton_rpc_list_dir (rpc, "/", NULL, &error);
  g_assert_no_error (error);
  g_assert_nonnull (entries);
  g_assert_nonnull (entries[0]);
  g_assert_nonnull (entries[1]);
  g_assert_null   (entries[2]);

  g_assert_cmpstr  (entries[0]->name,   ==, "Documents");
  g_assert_true    (entries[0]->is_dir);
  g_assert_cmpint  (entries[0]->mtime,  ==, 1700000000);

  g_assert_cmpstr  (entries[1]->name,   ==, "photo.jpg");
  g_assert_false   (entries[1]->is_dir);
  g_assert_cmpint  (entries[1]->size,   ==, 204800);

  proton_entry_free (entries[0]);
  proton_entry_free (entries[1]);
  g_free (entries);
  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_stat (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"result\":{\"name\":\"notes.txt\",\"is_dir\":false,\"size\":42,\"mtime\":1700000002}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error = NULL;
  ProtonRpc *rpc   = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  ProtonEntry *e = proton_rpc_stat (rpc, "/notes.txt", NULL, &error);
  g_assert_no_error (error);
  g_assert_nonnull (e);
  g_assert_cmpstr (e->name,  ==, "notes.txt");
  g_assert_false  (e->is_dir);
  g_assert_cmpint (e->size,  ==, 42);
  g_assert_cmpint (e->mtime, ==, 1700000002);

  proton_entry_free (e);
  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_read_file (void)
{
  /* "hello" base64-encoded = "aGVsbG8=" */
  const gchar *responses[] = {
    "{\"id\":1,\"result\":{\"data\":\"aGVsbG8=\",\"eof\":true}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error = NULL;
  ProtonRpc *rpc   = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  guchar   buf[16] = {0};
  gboolean eof     = FALSE;
  gssize   n       = proton_rpc_read_file (rpc, "/notes.txt", 0, 16, buf, &eof, NULL, &error);

  g_assert_no_error (error);
  g_assert_cmpint (n, ==, 5);
  g_assert_true (eof);
  g_assert_cmpmem (buf, 5, "hello", 5);

  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

static void
test_not_found (void)
{
  const gchar *responses[] = {
    "{\"id\":1,\"error\":{\"code\":-32001,\"message\":\"not found: /missing.txt\"}}"
  };
  MockServer *srv = mock_server_start (responses, G_N_ELEMENTS (responses));

  GError    *error = NULL;
  ProtonRpc *rpc   = proton_rpc_new (srv->socket_path, &error);
  g_assert_nonnull (rpc);

  ProtonEntry *e = proton_rpc_stat (rpc, "/missing.txt", NULL, &error);
  g_assert_null (e);
  g_assert_nonnull (error);
  g_error_free (error);

  proton_rpc_free (rpc);
  mock_server_stop (srv);
}

/* ---------- main ---------- */

int
main (int argc, char **argv)
{
  g_test_init (&argc, &argv, NULL);

  g_test_add_func ("/proton-rpc/auth-success",    test_auth_success);
  g_test_add_func ("/proton-rpc/auth-failure",    test_auth_failure);
  g_test_add_func ("/proton-rpc/resume-session",  test_resume_session);
  g_test_add_func ("/proton-rpc/list-dir",        test_list_dir);
  g_test_add_func ("/proton-rpc/stat",            test_stat);
  g_test_add_func ("/proton-rpc/read-file",       test_read_file);
  g_test_add_func ("/proton-rpc/not-found",       test_not_found);

  return g_test_run ();
}
