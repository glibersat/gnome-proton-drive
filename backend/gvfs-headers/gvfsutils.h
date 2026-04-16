/* Vendored from gvfs 1.57.2 common/gvfsutils.h */
#ifndef __G_VFS_UTILS_H__
#define __G_VFS_UTILS_H__

G_BEGIN_DECLS

void         gvfs_randomize_string      (char    *str, int len);
gboolean     gvfs_have_session_bus      (void);
gboolean     gvfs_get_debug             (void);
void         gvfs_set_debug             (gboolean debugging);
void         gvfs_setup_debug_handler   (void);
gboolean     gvfs_is_ipv6               (const char *host);
gchar       *gvfs_get_socket_dir        (void);

G_END_DECLS

#endif /* __G_VFS_UTILS_H__ */
