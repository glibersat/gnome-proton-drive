/* Minimal stub for the auto-generated gvfsdbus.h. */

#ifndef __G_VFS_DBUS_H__
#define __G_VFS_DBUS_H__

#include <gio/gio.h>

G_BEGIN_DECLS

typedef GDBusInterfaceSkeleton GVfsDBusMount;
typedef GDBusInterfaceSkeleton GVfsDBusMountable;

/* Spawner proxy — used by daemon-main.c to signal gvfsd after mount. */
typedef GDBusProxy GVfsDBusSpawner;

GType             gvfs_dbus_spawner_proxy_get_type        (void) G_GNUC_CONST;
GVfsDBusSpawner  *gvfs_dbus_spawner_proxy_new_for_bus_sync (GBusType               bus_type,
                                                            GDBusProxyFlags        flags,
                                                            const gchar           *name,
                                                            const gchar           *object_path,
                                                            GCancellable          *cancellable,
                                                            GError               **error);
void              gvfs_dbus_spawner_call_spawned           (GVfsDBusSpawner       *proxy,
                                                            gboolean               succeeded,
                                                            const gchar           *error_message,
                                                            guint32                error_code,
                                                            GCancellable          *cancellable,
                                                            GAsyncReadyCallback    callback,
                                                            gpointer               user_data);
gboolean          gvfs_dbus_spawner_call_spawned_finish    (GVfsDBusSpawner       *proxy,
                                                            GAsyncResult          *res,
                                                            GError               **error);

G_END_DECLS

#endif /* __G_VFS_DBUS_H__ */
