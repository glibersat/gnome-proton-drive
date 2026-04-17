/* Bus name claimed by this daemon.  The .monitor descriptor must match. */
const string BUS_NAME = "org.gnome.GVfs.VolumeMonitor.Proton";
const string OBJ_PATH = "/org/gtk/vfs/remotevolumemonitor";

void main (string[] args) {
    var loop    = new GLib.MainLoop (null, false);
    var cancel  = new GLib.Cancellable ();

    var monitor = new Proton.RemoteVolumeMonitor ();

    GLib.Bus.own_name (
        GLib.BusType.SESSION,
        BUS_NAME,
        GLib.BusNameOwnerFlags.NONE,
        (conn) => on_bus_acquired (conn, monitor, cancel, loop),
        null,
        () => {
            GLib.critical ("Could not acquire D-Bus name %s — another instance running?", BUS_NAME);
            loop.quit ();
        }
    );

    loop.run ();
}

void on_bus_acquired (GLib.DBusConnection    conn,
                      Proton.RemoteVolumeMonitor monitor,
                      GLib.Cancellable       cancel,
                      GLib.MainLoop          loop)
{
    try {
        conn.register_object (OBJ_PATH, monitor);
    } catch (GLib.IOError e) {
        GLib.critical ("Failed to register RemoteVolumeMonitor: %s", e.message);
        loop.quit ();
        return;
    }

    monitor.start.begin (cancel, (obj, res) => {
        try {
            monitor.start.end (res);
        } catch (GLib.Error e) {
            GLib.critical ("ProtonVolumeMonitor start failed: %s", e.message);
            loop.quit ();
        }
    });
}
