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

gboolean    proton_rpc_auth         (ProtonRpc    *rpc,
                                     const gchar  *username,
                                     const gchar  *password,
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

G_END_DECLS
