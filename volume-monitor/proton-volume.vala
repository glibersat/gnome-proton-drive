/**
 * Models one Proton Drive account as a GVfs remote volume.
 *
 * Produces the a{sv} variant dict that org.gtk.vfs.RemoteVolumeMonitor
 * expects in VolumeAdded signals and List replies.
 */
public class Proton.Volume : GLib.Object {

    public string username { get; construct; }

    /* Stable per-account ID — used as the RemoteVolumeMonitor dbus_id */
    public string id { get; private set; }

    /* Activation URI triggers gvfsd-proton via the proton:// scheme */
    public string activation_uri { get; private set; }

    public Volume (string username) {
        Object (username: username);
    }

    construct {
        id             = "proton-%s".printf (username);
        activation_uri = "proton://%s/".printf (GLib.Uri.escape_string (username, null, false));
    }

    /**
     * Serialise to the a{sv} dict understood by GProxyVolumeMonitor.
     * Keys are defined in gvfs/monitor/proxy/gvfsproxyvolumemonitordaemon.c.
     */
    public GLib.Variant to_variant () {
        var builder = new GLib.VariantBuilder (new GLib.VariantType ("a{sv}"));

        builder.add ("{sv}", "type",             new GLib.Variant.string ("volume"));
        builder.add ("{sv}", "id",               new GLib.Variant.string (id));
        builder.add ("{sv}", "name",             new GLib.Variant.string (display_name ()));
        builder.add ("{sv}", "gicon-serialized", icon_variant ());
        builder.add ("{sv}", "uuid",             new GLib.Variant.string (id));
        builder.add ("{sv}", "activation-uri",   new GLib.Variant.string (activation_uri));
        builder.add ("{sv}", "can-mount",        new GLib.Variant.boolean (true));
        builder.add ("{sv}", "can-eject",        new GLib.Variant.boolean (false));
        builder.add ("{sv}", "should-automount", new GLib.Variant.boolean (false));

        return builder.end ();
    }

    /* ------------------------------------------------------------------ */

    private string display_name () {
        return "Proton Drive (%s)".printf (username);
    }

    /* Serialise a ThemedIcon so GIO can reconstruct it on the client side. */
    private GLib.Variant icon_variant () {
        var icon   = new GLib.ThemedIcon ("proton-drive-symbolic");
        var serial = icon.serialize ();
        /* serialize() can return null for unusual icon types; fall back to
         * a plain string icon so the volume at least appears with no icon. */
        return serial ?? new GLib.Variant.string ("proton-drive-symbolic");
    }
}
