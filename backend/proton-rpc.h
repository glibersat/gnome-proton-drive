#pragma once

#include <glib.h>
#include <gio/gio.h>

G_BEGIN_DECLS

/* Synchronous JSON-RPC client over a Unix socket.
 * Each call blocks until the matching response arrives.
 * Thread-safe: a mutex serialises concurrent callers. */

typedef struct _ProtonRpc ProtonRpc;

typedef struct {
  gchar    *name;
  gboolean  is_dir;
  gint64    size;
  gint64    mtime;   /* unix seconds */
} ProtonEntry;

ProtonRpc  *proton_rpc_new          (const gchar  *socket_path,
                                     GError      **error);
void        proton_rpc_free         (ProtonRpc    *rpc);

/* Full SRP login — only called from the setup wizard, not from do_mount.
 * On success the returned AuthResult fields should be stored in libsecret. */
gboolean    proton_rpc_auth         (ProtonRpc    *rpc,
                                     const gchar  *username,
                                     const gchar  *password,
                                     gchar       **out_uid,
                                     gchar       **out_refresh_token,
                                     GBytes      **out_salted_passphrase,
                                     GError      **error);

/* Resume a session from stored credentials — no password needed.
 * Called by do_mount on every mount. On success, out_new_uid and
 * out_new_refresh_token hold the rotated credentials that must be
 * persisted back to libsecret. */
gboolean    proton_rpc_resume_session (ProtonRpc    *rpc,
                                       const gchar  *uid,
                                       const gchar  *refresh_token,
                                       GBytes       *salted_passphrase,
                                       gchar       **out_new_uid,
                                       gchar       **out_new_refresh_token,
                                       GError      **error);

/* Returns a NULL-terminated array of ProtonEntry* owned by the caller.
 * Free each entry with proton_entry_free(), then g_free() the array. */
ProtonEntry **proton_rpc_list_dir   (ProtonRpc    *rpc,
                                     const gchar  *path,
                                     GError      **error);

ProtonEntry  *proton_rpc_stat       (ProtonRpc    *rpc,
                                     const gchar  *path,
                                     GError      **error);

/* Read up to @length bytes from @path at @offset.
 * Returns the number of bytes read into @buf, or -1 on error.
 * Sets *@eof when the end of file has been reached. */
gssize        proton_rpc_read_file  (ProtonRpc    *rpc,
                                     const gchar  *path,
                                     gint64        offset,
                                     gint64        length,
                                     guchar       *buf,
                                     gboolean     *eof,
                                     GError      **error);

void          proton_entry_free     (ProtonEntry  *entry);

/* A single remote-change event returned by GetEvents.
 * path may be NULL when the changed link was not yet visited in this session. */
typedef struct {
  gchar *type;    /* "changed" | "deleted" | "created" */
  gchar *link_id;
  gchar *path;    /* absolute POSIX path, or NULL */
} ProtonEvent;

/* Drain the helper's event queue.  Returns a NULL-terminated array of
 * ProtonEvent* owned by the caller.  Never blocks — returns an empty array
 * (length 0, first element NULL) when nothing is pending.
 * Free with proton_events_free(). */
ProtonEvent **proton_rpc_get_events (ProtonRpc *rpc, GError **error);
void          proton_event_free     (ProtonEvent *event);
void          proton_events_free    (ProtonEvent **events);

G_END_DECLS
