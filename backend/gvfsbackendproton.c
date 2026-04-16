#include "gvfsbackendproton.h"
#include "proton-rpc.h"

#include <gvfsjobmount.h>
#include <gvfsjobqueryinfo.h>
#include <gvfsjobenumerate.h>
#include <gvfsjobopenforread.h>
#include <gvfsjobread.h>
#include <gvfsjobcloseread.h>


/* Per-open-file handle: tracks path and current read offset. */
typedef struct {
  gchar  *path;
  gint64  offset;
} ProtonHandle;

struct _GVfsBackendProton {
  GVfsBackend  parent_instance;
  ProtonRpc   *rpc;
};

struct _GVfsBackendProtonClass {
  GVfsBackendClass parent_class;
};

G_DEFINE_TYPE (GVfsBackendProton, g_vfs_backend_proton, G_VFS_TYPE_BACKEND)

/* ---------- helpers ---------- */

static void
fill_file_info (GFileInfo *info, ProtonEntry *e)
{
  g_file_info_set_name (info, e->name);
  g_file_info_set_display_name (info, e->name);
  g_file_info_set_edit_name (info, e->name);

  if (e->is_dir)
    {
      g_file_info_set_file_type (info, G_FILE_TYPE_DIRECTORY);
      g_file_info_set_content_type (info, "inode/directory");
    }
  else
    {
      g_file_info_set_file_type (info, G_FILE_TYPE_REGULAR);
      g_file_info_set_size (info, e->size);
    }

  GDateTime *dt = g_date_time_new_from_unix_utc (e->mtime);
  g_file_info_set_modification_date_time (info, dt);
  g_date_time_unref (dt);
}

/* ---------- helper spawn ---------- */

/* Locate proton-drive-helper: check libexecdir first (both installed there),
 * then fall back to PATH for development builds. */
static gchar *
find_helper_binary (void)
{
  gchar *candidate = g_build_filename (PROTON_LIBEXECDIR, "proton-drive-helper", NULL);
  if (g_file_test (candidate, G_FILE_TEST_IS_EXECUTABLE))
    return candidate;
  g_free (candidate);
  return g_find_program_in_path ("proton-drive-helper");
}

/* Poll until the Unix socket file appears or we time out. */
static gboolean
wait_for_socket (const gchar *path, guint timeout_ms)
{
  for (guint elapsed = 0; elapsed < timeout_ms; elapsed += 50)
    {
      if (g_file_test (path, G_FILE_TEST_EXISTS))
        return TRUE;
      g_usleep (50 * G_USEC_PER_SEC / 1000);
    }
  return FALSE;
}

/* ---------- vtable ---------- */

static void
do_mount (GVfsBackend  *backend,
          GVfsJobMount *job,
          GMountSpec   *mount_spec,
          GMountSource *mount_source,
          gboolean      is_automount)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (backend);

  const gchar *account  = g_mount_spec_get (mount_spec, "account");
  const gchar *username = g_mount_spec_get (mount_spec, "username");

  gchar *socket_path = g_strdup_printf ("/run/user/%u/proton-drive-%s.sock",
                                        getuid (), account ? account : "default");

  /* Spawn the helper if its socket isn't already listening. */
  if (!g_file_test (socket_path, G_FILE_TEST_EXISTS))
    {
      if (!account)
        {
          g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                    G_IO_ERROR_INVALID_ARGUMENT,
                                    "mount spec missing required 'account' key");
          g_free (socket_path);
          return;
        }

      gchar *helper = find_helper_binary ();
      if (!helper)
        {
          g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                    G_IO_ERROR_NOT_FOUND,
                                    "proton-drive-helper not found in libexecdir or PATH");
          g_free (socket_path);
          return;
        }

      GError *spawn_error = NULL;
      gchar *spawn_argv[] = { helper, "--socket", socket_path,
                               "--account", (gchar *) account, NULL };
      g_spawn_async (NULL, spawn_argv, NULL,
                     G_SPAWN_DEFAULT, NULL, NULL, NULL, &spawn_error);
      g_free (helper);

      if (spawn_error)
        {
          g_vfs_job_failed_from_error (G_VFS_JOB (job), spawn_error);
          g_error_free (spawn_error);
          g_free (socket_path);
          return;
        }

      if (!wait_for_socket (socket_path, 5000))
        {
          g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                    G_IO_ERROR_TIMED_OUT,
                                    "timed out waiting for proton-drive-helper to start");
          g_free (socket_path);
          return;
        }
    }

  GError *error = NULL;
  self->rpc = proton_rpc_new (socket_path, &error);
  g_free (socket_path);

  if (!self->rpc)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }

  /* Retrieve the password from libsecret via secret-tool.
   * This keeps libsecret out of the C backend's direct dependencies for now;
   * replace with a direct libsecret call in a follow-up. */
  gchar *password = NULL;
  if (username)
    {
      gchar *argv[] = {
        "secret-tool", "lookup",
        "schema", "org.gnome.proton.drive",
        "username", (gchar *) username,
        NULL
      };
      gint exit_status;
      g_spawn_sync (NULL, argv, NULL,
                    G_SPAWN_SEARCH_PATH | G_SPAWN_STDERR_TO_DEV_NULL,
                    NULL, NULL, &password, NULL, &exit_status, &error);
      if (error)
        {
          g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
          g_error_free (error);
          return;
        }
      /* strip trailing newline from secret-tool output */
      g_strchomp (password);
    }

  if (!proton_rpc_auth (self->rpc, username ? username : "", password ? password : "", &error))
    {
      g_free (password);
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }
  g_free (password);

  GMountSpec *spec = g_mount_spec_new ("proton");
  if (account)
    g_mount_spec_set (spec, "account", account);
  if (username)
    g_mount_spec_set (spec, "username", username);

  g_vfs_backend_set_mount_spec (backend, spec);
  g_mount_spec_unref (spec);
  g_vfs_backend_set_display_name (backend, "Proton Drive");
  g_vfs_backend_set_icon_name (backend, "folder-remote");
  g_vfs_backend_set_symbolic_icon_name (backend, "folder-remote-symbolic");
  g_vfs_backend_set_user_visible (backend, TRUE);

  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_query_info (GVfsBackend           *backend,
               GVfsJobQueryInfo      *job,
               const gchar           *filename,
               GFileQueryInfoFlags    flags,
               GFileInfo             *info,
               GFileAttributeMatcher *matcher)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (backend);
  GError *error = NULL;

  ProtonEntry *e = proton_rpc_stat (self->rpc, filename, &error);
  if (!e)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }

  fill_file_info (info, e);
  proton_entry_free (e);
  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_enumerate (GVfsBackend           *backend,
              GVfsJobEnumerate      *job,
              const gchar           *filename,
              GFileAttributeMatcher *matcher,
              GFileQueryInfoFlags    flags)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (backend);
  GError *error = NULL;

  ProtonEntry **entries = proton_rpc_list_dir (self->rpc, filename, &error);
  if (!entries)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }

  for (guint i = 0; entries[i]; i++)
    {
      g_autoptr(GFileInfo) info = g_file_info_new ();
      fill_file_info (info, entries[i]);
      g_vfs_job_enumerate_add_info (job, info);
      proton_entry_free (entries[i]);
    }
  g_free (entries);

  g_vfs_job_enumerate_done (job);
  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_open_for_read (GVfsBackend        *backend,
                  GVfsJobOpenForRead *job,
                  const gchar        *filename)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (backend);
  GError *error = NULL;

  /* Verify the path exists and is a file before handing back a handle. */
  ProtonEntry *e = proton_rpc_stat (self->rpc, filename, &error);
  if (!e)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }
  if (e->is_dir)
    {
      proton_entry_free (e);
      g_vfs_job_failed (G_VFS_JOB (job), G_IO_ERROR, G_IO_ERROR_IS_DIRECTORY,
                        "Is a directory");
      return;
    }
  proton_entry_free (e);

  ProtonHandle *h = g_new0 (ProtonHandle, 1);
  h->path   = g_strdup (filename);
  h->offset = 0;

  g_vfs_job_open_for_read_set_handle (job, h);
  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_read (GVfsBackend      *backend,
         GVfsJobRead      *job,
         GVfsBackendHandle handle,
         gchar            *buffer,
         gsize             bytes_requested)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (backend);
  ProtonHandle      *h    = handle;
  GError            *error = NULL;
  gboolean           eof   = FALSE;

  gssize n = proton_rpc_read_file (self->rpc, h->path, h->offset,
                                   (gint64) bytes_requested,
                                   (guchar *) buffer, &eof, &error);
  if (n < 0)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }

  h->offset += n;
  g_vfs_job_read_set_size (job, (gsize) n);
  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_close_read (GVfsBackend      *backend,
               GVfsJobCloseRead *job,
               GVfsBackendHandle handle)
{
  ProtonHandle *h = handle;
  g_free (h->path);
  g_free (h);
  g_vfs_job_succeeded (G_VFS_JOB (job));
}

/* ---------- GObject boilerplate ---------- */

static void
g_vfs_backend_proton_finalize (GObject *object)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (object);
  proton_rpc_free (self->rpc);
  G_OBJECT_CLASS (g_vfs_backend_proton_parent_class)->finalize (object);
}

static void
g_vfs_backend_proton_init (GVfsBackendProton *self)
{
}

static void
g_vfs_backend_proton_class_init (GVfsBackendProtonClass *klass)
{
  GObjectClass    *obj_class     = G_OBJECT_CLASS (klass);
  GVfsBackendClass *backend_class = G_VFS_BACKEND_CLASS (klass);

  obj_class->finalize      = g_vfs_backend_proton_finalize;
  backend_class->mount     = do_mount;
  backend_class->query_info = do_query_info;
  backend_class->enumerate  = do_enumerate;
  backend_class->open_for_read = do_open_for_read;
  backend_class->read          = do_read;
  backend_class->close_read    = do_close_read;
}

