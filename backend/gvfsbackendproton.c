#include "gvfsbackendproton.h"
#include "proton-rpc.h"

#include <gvfsjobmount.h>
#include <gvfsjobqueryinfo.h>
#include <gvfsjobenumerate.h>
#include <gvfsjobopenforread.h>
#include <gvfsjobread.h>
#include <gvfsjobcloseread.h>
#include <gvfsmonitor.h>
#include <gvfsjobcreatemonitor.h>

/* How often (seconds) the backend polls GetEvents from the helper. */
#define POLL_INTERVAL_S 5

/* Per-open-file handle: tracks path and current read offset. */
typedef struct {
  gchar  *path;
  gint64  offset;
} ProtonHandle;

/* Weak-reference wrapper for a GVfsMonitor stored in self->monitors.
 * monitor is zeroed by GLib when the underlying object is finalised. */
typedef struct {
  gchar       *path;
  GVfsMonitor *monitor; /* weak ref — may become NULL */
} ProtonMonitorEntry;

struct _GVfsBackendProton {
  GVfsBackend  parent_instance;
  ProtonRpc   *rpc;
  GPtrArray   *monitors; /* ProtonMonitorEntry*, weak refs to GVfsMonitor */
  guint        poll_id;  /* GLib timeout source id, 0 when inactive */
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
      GIcon *icon = g_themed_icon_new ("folder");
      GIcon *sicon = g_themed_icon_new ("folder-symbolic");
      g_file_info_set_icon (info, icon);
      g_file_info_set_symbolic_icon (info, sicon);
      g_object_unref (icon);
      g_object_unref (sicon);
    }
  else
    {
      g_file_info_set_file_type (info, G_FILE_TYPE_REGULAR);
      g_file_info_set_size (info, e->size);
      gchar *content_type = g_content_type_guess (e->name, NULL, 0, NULL);
      if (content_type)
        {
          g_file_info_set_content_type (info, content_type);
          GIcon *icon  = g_content_type_get_icon (content_type);
          GIcon *sicon = g_content_type_get_symbolic_icon (content_type);
          if (icon)  { g_file_info_set_icon (info, icon);           g_object_unref (icon); }
          if (sicon) { g_file_info_set_symbolic_icon (info, sicon); g_object_unref (sicon); }
          g_free (content_type);
        }
    }

  GDateTime *dt = g_date_time_new_from_unix_utc (e->mtime);
  g_file_info_set_modification_date_time (info, dt);
  g_date_time_unref (dt);
}

/* ---------- libsecret helpers ---------- */

/* Store @secret under the given field for @username via secret-tool,
 * writing the secret to the subprocess stdin so it never appears in argv. */
static void
secret_tool_store (const gchar *username, const gchar *field, const gchar *secret)
{
  gchar *label = g_strdup_printf ("Proton Drive %s (%s)", field, username);
  const gchar *argv[] = {
    "secret-tool", "store", "--label", label,
    "schema",   "org.gnome.proton.drive",
    "username", username,
    "field",    field,
    NULL
  };

  GError *err = NULL;
  GSubprocess *proc = g_subprocess_newv (argv,
                                         G_SUBPROCESS_FLAGS_STDIN_PIPE |
                                         G_SUBPROCESS_FLAGS_STDERR_SILENCE,
                                         &err);
  g_free (label);
  if (!proc)
    {
      g_warning ("secret-tool store: %s", err->message);
      g_error_free (err);
      return;
    }

  g_subprocess_communicate_utf8 (proc, secret, NULL, NULL, NULL, &err);
  if (err)
    {
      g_warning ("secret-tool store write: %s", err->message);
      g_error_free (err);
    }
  g_object_unref (proc);
}

/* ---------- monitor helpers ---------- */

static void
proton_monitor_entry_free (gpointer data)
{
  ProtonMonitorEntry *entry = data;
  if (entry->monitor)
    g_object_remove_weak_pointer (G_OBJECT (entry->monitor),
                                  (gpointer *) &entry->monitor);
  g_free (entry->path);
  g_free (entry);
}

/* Emit event_type on every monitor whose path matches file_path or is a
 * direct parent of it (directory monitor is notified when a child changes).
 * When file_path is NULL, emit on all monitors (full-refresh fallback). */
static void
emit_on_monitors (GVfsBackendProton *self,
                  GFileMonitorEvent  event_type,
                  const gchar       *file_path)
{
  for (guint i = 0; i < self->monitors->len; i++)
    {
      ProtonMonitorEntry *entry = g_ptr_array_index (self->monitors, i);
      if (!entry->monitor)
        continue; /* finalised */

      gboolean match = (file_path == NULL)
        || g_str_equal (entry->path, file_path)
        || g_str_has_prefix (file_path, entry->path);

      if (match)
        g_vfs_monitor_emit_event (entry->monitor, event_type,
                                  file_path ? file_path : entry->path, NULL);
    }
}

/* GLib timeout callback — runs on the GLib main loop every POLL_INTERVAL_S. */
static gboolean
poll_events_cb (gpointer user_data)
{
  GVfsBackendProton *self = G_VFS_BACKEND_PROTON (user_data);
  GError *error = NULL;

  ProtonEvent **events = proton_rpc_get_events (self->rpc, &error);
  if (!events)
    {
      if (error)
        {
          g_debug ("poll_events: %s", error->message);
          g_error_free (error);
        }
      return G_SOURCE_CONTINUE;
    }

  for (ProtonEvent **ev = events; *ev; ev++)
    {
      GFileMonitorEvent gev;
      if (g_str_equal ((*ev)->type, "deleted"))
        gev = G_FILE_MONITOR_EVENT_DELETED;
      else if (g_str_equal ((*ev)->type, "created"))
        gev = G_FILE_MONITOR_EVENT_CREATED;
      else
        gev = G_FILE_MONITOR_EVENT_CHANGED;

      emit_on_monitors (self, gev, (*ev)->path);
    }

  proton_events_free (events);
  return G_SOURCE_CONTINUE;
}

/* ---------- monitor vtable ---------- */

static void
register_monitor (GVfsBackendProton    *self,
                  GVfsJobCreateMonitor *job,
                  const gchar          *filename)
{
  GVfsMonitor *monitor = g_vfs_monitor_new (G_VFS_BACKEND (self));
  g_vfs_job_create_monitor_set_monitor (job, monitor);

  ProtonMonitorEntry *entry = g_new0 (ProtonMonitorEntry, 1);
  entry->path    = g_strdup (filename);
  entry->monitor = monitor;
  g_object_add_weak_pointer (G_OBJECT (monitor), (gpointer *) &entry->monitor);

  g_ptr_array_add (self->monitors, entry);
  g_object_unref (monitor); /* entry holds a weak ref; job holds the strong ref */

  g_vfs_job_succeeded (G_VFS_JOB (job));
}

static void
do_create_dir_monitor (GVfsBackend          *backend,
                       GVfsJobCreateMonitor *job,
                       const gchar          *filename,
                       GFileMonitorFlags     flags G_GNUC_UNUSED)
{
  register_monitor (G_VFS_BACKEND_PROTON (backend), job, filename);
}

static void
do_create_file_monitor (GVfsBackend          *backend,
                        GVfsJobCreateMonitor *job,
                        const gchar          *filename,
                        GFileMonitorFlags     flags G_GNUC_UNUSED)
{
  register_monitor (G_VFS_BACKEND_PROTON (backend), job, filename);
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

  /* GVfs maps URI host → "host" mount spec key; we store the full email
   * address there (@ encoded as %40 in the URI so GVfs passes it through).
   * URI: proton://glibersat%40sigill.org/ */
  const gchar *account = g_mount_spec_get (mount_spec, "host");

  if (!account || *account == '\0')
    {
      g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                G_IO_ERROR_INVALID_ARGUMENT,
                                "URI must contain the account email as host, "
                                "e.g. proton://you%40example.com/");
      return;
    }

  gchar *socket_path = g_strdup_printf ("/run/user/%u/proton-drive-%s.sock",
                                        getuid (), account);

  GError *error = NULL;

  /* Try to connect to an already-running helper first.  If that fails
   * (socket missing or stale), spawn a fresh one and retry. */
  self->rpc = proton_rpc_new (socket_path, &error);
  if (!self->rpc)
    {
      g_clear_error (&error);

      gchar *helper = find_helper_binary ();
      if (!helper)
        {
          g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                    G_IO_ERROR_NOT_FOUND,
                                    "proton-drive-helper not found in libexecdir or PATH");
          g_free (socket_path);
          return;
        }

      /* Remove a stale socket file so the helper can bind cleanly. */
      unlink (socket_path);

      GError *spawn_error = NULL;
      gchar *spawn_argv[] = { helper, "--socket", socket_path, NULL };
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

      self->rpc = proton_rpc_new (socket_path, &error);
    }

  g_free (socket_path);

  if (!self->rpc)
    {
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
      return;
    }

  /* Fetch the three stored credentials from libsecret via secret-tool.
   * The keyring holds uid, refresh_token, and salted_passphrase — not the
   * raw password, which is never stored. */
  gchar *uid               = NULL;
  gchar *refresh_token     = NULL;
  gchar *salted_pass_b64   = NULL;

  {
    gint exit_status;

#define LOOKUP_FIELD(field, out) \
    { \
      gchar *argv[] = { "secret-tool", "lookup", \
                        "schema",   "org.gnome.proton.drive", \
                        "username", (gchar *) account, \
                        "field",    field, NULL }; \
      g_spawn_sync (NULL, argv, NULL, \
                    G_SPAWN_SEARCH_PATH | G_SPAWN_STDERR_TO_DEV_NULL, \
                    NULL, NULL, &(out), NULL, &exit_status, &error); \
      if (error) goto cred_error; \
      g_strchomp (out); \
    }

    LOOKUP_FIELD ("uid",               uid)
    LOOKUP_FIELD ("refresh_token",     refresh_token)
    LOOKUP_FIELD ("salted_passphrase", salted_pass_b64)
#undef LOOKUP_FIELD
  }

  if (!uid || !refresh_token || !salted_pass_b64)
    {
      g_vfs_job_failed_literal (G_VFS_JOB (job), G_IO_ERROR,
                                G_IO_ERROR_NOT_INITIALIZED,
                                "no stored credentials — run the Proton Drive setup wizard first");
      goto cred_cleanup;
    }

  {
    gsize   sp_len  = 0;
    guchar *sp_data = g_base64_decode (salted_pass_b64, &sp_len);
    GBytes *sp      = g_bytes_new_take (sp_data, sp_len);

    gchar *new_uid           = NULL;
    gchar *new_refresh_token = NULL;

    gboolean ok = proton_rpc_resume_session (self->rpc, uid, refresh_token, sp,
                                             &new_uid, &new_refresh_token, &error);
    g_bytes_unref (sp);

    if (!ok)
      {
        g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
        g_error_free (error);
        goto cred_cleanup;
      }

    /* Proton rotates the refresh token on every use — persist the new one. */
    if (new_uid && new_refresh_token)
      secret_tool_store (account, "uid",           new_uid);
    if (new_refresh_token)
      secret_tool_store (account, "refresh_token", new_refresh_token);
    g_free (new_uid);
    g_free (new_refresh_token);
  }

  g_free (uid);
  g_free (refresh_token);
  g_free (salted_pass_b64);

  if (FALSE)
    {
cred_error:
      g_vfs_job_failed_from_error (G_VFS_JOB (job), error);
      g_error_free (error);
cred_cleanup:
      g_free (uid);
      g_free (refresh_token);
      g_free (salted_pass_b64);
      return;
    }

  GMountSpec *spec = g_mount_spec_new ("proton");
  g_mount_spec_set (spec, "host", account);

  g_vfs_backend_set_mount_spec (backend, spec);
  g_mount_spec_unref (spec);
  gchar *display = g_strdup_printf ("Proton Drive (%s)", account);
  g_vfs_backend_set_display_name (backend, display);
  g_free (display);
  g_vfs_backend_set_icon_name (backend, "folder-remote");
  g_vfs_backend_set_symbolic_icon_name (backend, "folder-remote-symbolic");
  g_vfs_backend_set_user_visible (backend, TRUE);

  g_vfs_job_succeeded (G_VFS_JOB (job));

  /* Start the event-polling timer now that the RPC connection is live. */
  self->poll_id = g_timeout_add_seconds (POLL_INTERVAL_S, poll_events_cb, self);
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
  if (self->poll_id)
    g_source_remove (self->poll_id);
  g_ptr_array_unref (self->monitors);
  proton_rpc_free (self->rpc);
  G_OBJECT_CLASS (g_vfs_backend_proton_parent_class)->finalize (object);
}

static void
g_vfs_backend_proton_init (GVfsBackendProton *self)
{
  self->monitors = g_ptr_array_new_with_free_func (proton_monitor_entry_free);
}

static void
g_vfs_backend_proton_class_init (GVfsBackendProtonClass *klass)
{
  GObjectClass    *obj_class     = G_OBJECT_CLASS (klass);
  GVfsBackendClass *backend_class = G_VFS_BACKEND_CLASS (klass);

  obj_class->finalize      = g_vfs_backend_proton_finalize;
  backend_class->mount              = do_mount;
  backend_class->query_info         = do_query_info;
  backend_class->enumerate          = do_enumerate;
  backend_class->open_for_read      = do_open_for_read;
  backend_class->read               = do_read;
  backend_class->close_read         = do_close_read;
  backend_class->create_dir_monitor  = do_create_dir_monitor;
  backend_class->create_file_monitor = do_create_file_monitor;
}

