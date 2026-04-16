/* Minimal stub of gvfsdaemon.h for out-of-tree builds.
 * Only declares GVfsDaemon as an opaque GObject type, which is all
 * gvfsbackend.h needs from this header. */

#ifndef __G_VFS_DAEMON_H__
#define __G_VFS_DAEMON_H__

#include <glib-object.h>
#include <gio/gio.h>
#include <gvfstypes.h>
#include <gmountsource.h>

G_BEGIN_DECLS

#define G_VFS_TYPE_DAEMON         (g_vfs_daemon_get_type ())
#define G_VFS_DAEMON(o)           (G_TYPE_CHECK_INSTANCE_CAST ((o), G_VFS_TYPE_DAEMON, GVfsDaemon))
#define G_VFS_IS_DAEMON(o)        (G_TYPE_CHECK_INSTANCE_TYPE ((o), G_VFS_TYPE_DAEMON))

typedef struct _GVfsDaemon      GVfsDaemon;
typedef struct _GVfsDaemonClass GVfsDaemonClass;

GType       g_vfs_daemon_get_type          (void) G_GNUC_CONST;
GVfsDaemon *g_vfs_daemon_new               (gboolean main_daemon, gboolean replace);
void        g_vfs_daemon_set_max_threads   (GVfsDaemon *daemon, gint max_threads);
void        g_vfs_daemon_initiate_mount    (GVfsDaemon *daemon,
                                            GMountSpec *mount_spec,
                                            GMountSource *mount_source,
                                            gboolean is_automount,
                                            gpointer object,
                                            gpointer invocation);
void        g_vfs_register_backend         (GType backend_type, const char *type);

G_END_DECLS

#endif /* __G_VFS_DAEMON_H__ */
