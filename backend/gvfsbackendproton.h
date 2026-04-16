#pragma once

#include <gvfsbackend.h>

G_BEGIN_DECLS

#define G_VFS_TYPE_BACKEND_PROTON         (g_vfs_backend_proton_get_type ())
#define G_VFS_BACKEND_PROTON(o)           (G_TYPE_CHECK_INSTANCE_CAST ((o), G_VFS_TYPE_BACKEND_PROTON, GVfsBackendProton))
#define G_VFS_IS_BACKEND_PROTON(o)        (G_TYPE_CHECK_INSTANCE_TYPE ((o), G_VFS_TYPE_BACKEND_PROTON))

typedef struct _GVfsBackendProton      GVfsBackendProton;
typedef struct _GVfsBackendProtonClass GVfsBackendProtonClass;

GType g_vfs_backend_proton_get_type (void) G_GNUC_CONST;

G_END_DECLS
