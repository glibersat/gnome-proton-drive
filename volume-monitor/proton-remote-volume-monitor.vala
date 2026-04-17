/**
 * Implements the org.gtk.vfs.RemoteVolumeMonitor D-Bus interface.
 *
 * GIO's GProxyVolumeMonitor connects to this daemon and forwards volume
 * lifecycle events to every GVolumeMonitor client in the session.
 *
 * Only the subset of the interface needed for read-only volume discovery
 * is implemented.  Mount/unmount operations are delegated to GVfs via the
 * activation URI (proton://), which causes gvfsd-proton to be spawned.
 */
[DBus (name = "org.gtk.vfs.RemoteVolumeMonitor")]
public class Proton.RemoteVolumeMonitor : GLib.Object {

    /* Emitted as D-Bus signals — names match the interface exactly. */
    public signal void volume_added   (string dbus_id, GLib.Variant volume);
    public signal void volume_removed (string dbus_id, GLib.Variant volume);

    /* Unused in read-only mode but required by the interface. */
    public signal void mount_added    (string dbus_id, GLib.Variant mount);
    public signal void mount_removed  (string dbus_id, GLib.Variant mount);
    public signal void drive_added    (string dbus_id, GLib.Variant drive);
    public signal void drive_removed  (string dbus_id, GLib.Variant drive);
    public signal void drive_changed  (string dbus_id, GLib.Variant drive);
    public signal void volume_changed (string dbus_id, GLib.Variant volume);
    public signal void mount_changed  (string dbus_id, GLib.Variant mount);
    public signal void mount_op_ask_password  (string dbus_id, string message,
                                               string default_user, string default_domain,
                                               uint   flags);
    public signal void mount_op_ask_question  (string dbus_id, string message,
                                               string[] choices);
    public signal void mount_op_show_processes (string dbus_id, string message,
                                                int32[] pids, string[] choices);
    public signal void mount_op_aborted       (string dbus_id);
    public signal void mount_pre_unmount      (string dbus_id, GLib.Variant mount);

    private GLib.HashTable<string, Proton.Volume> _volumes;
    private Proton.AccountWatcher                 _watcher;

    public RemoteVolumeMonitor () {
        _volumes = new GLib.HashTable<string, Proton.Volume> (str_hash, str_equal);
        _watcher = new Proton.AccountWatcher ();
        _watcher.account_added.connect   (on_account_added);
        _watcher.account_removed.connect (on_account_removed);
    }

    public async void start (GLib.Cancellable? cancel = null) throws GLib.Error {
        yield _watcher.start (cancel);
    }

    /* ------------------------------------------------------------------ */
    /* org.gtk.vfs.RemoteVolumeMonitor methods                             */
    /* ------------------------------------------------------------------ */

    public bool is_supported () throws GLib.Error {
        return true;
    }

    /**
     * Called by GProxyVolumeMonitor on startup to reconstruct state.
     * Returns empty drives/mounts; we only expose volumes.
     */
    public void list (out GLib.Variant[] drives,
                      out GLib.Variant[] volumes,
                      out GLib.Variant[] mounts,
                      out bool           is_supported) throws GLib.Error
    {
        drives       = {};
        mounts       = {};
        is_supported = true;

        var vols = new GLib.Array<GLib.Variant> ();
        _volumes.foreach ((_, vol) => vols.append_val (vol.to_variant ()));
        volumes = vols.data;
    }

    public void cancel_operation (string cancellation_id,
                                  out bool was_cancelled) throws GLib.Error {
        was_cancelled = false;
    }

    /* Mount is handled by GVfs via the activation URI; we just acknowledge. */
    public void mount_unmounted_volume (string id,
                                        string cancellation_id,
                                        uint   mount_op_id) throws GLib.Error
    {
    }

    public void unmount_mount (string id,
                               string cancellation_id,
                               uint   mount_op_id,
                               uint   unmount_flags) throws GLib.Error
    {
    }

    public void eject_volume_with_operation (string id,
                                             string cancellation_id,
                                             uint   mount_op_id,
                                             uint   eject_flags) throws GLib.Error
    {
    }

    public void eject_drive_with_operation (string id,
                                            string cancellation_id,
                                            uint   mount_op_id,
                                            uint   eject_flags) throws GLib.Error
    {
    }

    public void mount_op_reply (string  mount_op_id,
                                int     result,
                                string  user_name,
                                string  domain,
                                string  encoded_password,
                                int     password_save,
                                int     choice,
                                bool    anonymous) throws GLib.Error
    {
    }

    /* ------------------------------------------------------------------ */
    /* Account watcher callbacks                                            */
    /* ------------------------------------------------------------------ */

    private void on_account_added (string username) {
        var vol = new Proton.Volume (username);
        _volumes.set (username, vol);
        volume_added (vol.id, vol.to_variant ());
        GLib.message ("ProtonVolumeMonitor: volume added for %s", username);
    }

    private void on_account_removed (string username) {
        var vol = _volumes.get (username);
        if (vol == null)
            return;
        _volumes.remove (username);
        volume_removed (vol.id, vol.to_variant ());
        GLib.message ("ProtonVolumeMonitor: volume removed for %s", username);
    }
}
