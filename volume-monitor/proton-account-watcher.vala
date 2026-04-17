/**
 * Watches for Proton Drive accounts via two paths:
 *   1. libsecret collection changes (catches stores from any source)
 *   2. org.gnome.ProtonDrive D-Bus signals from the setup wizard (fast path)
 *
 * Emits account_added / account_removed exactly once per transition.
 * The username is always the Proton account email address.
 */
public class Proton.AccountWatcher : GLib.Object {

    public signal void account_added   (string username);
    public signal void account_removed (string username);

    private Secret.Schema                    _schema;
    private Secret.Service                   _service;
    private GLib.DBusConnection              _bus;
    private GLib.HashTable<string, bool>     _known;

    private uint _sub_added;
    private uint _sub_removed;

    construct {
        _known = new GLib.HashTable<string, bool> (str_hash, str_equal);

        /* Schema.newv avoids varargs — safer from Vala. */
        var attr_types = new GLib.HashTable<string, Secret.SchemaAttributeType> (
            str_hash, str_equal);
        attr_types["username"] = Secret.SchemaAttributeType.STRING;
        attr_types["field"]    = Secret.SchemaAttributeType.STRING;
        _schema = new Secret.Schema.newv (
            "org.gnome.proton.drive", Secret.SchemaFlags.NONE, attr_types);
    }

    /**
     * Open the keyring service, do an initial account scan, then arm all
     * change listeners.  Call once at daemon startup.
     */
    public async void start (GLib.Cancellable? cancel = null) throws GLib.Error {
        /* null service_bus_name → use the well-known default session keyring. */
        _service = yield Secret.Service.open (
            typeof (Secret.Service),
            null,
            Secret.ServiceFlags.OPEN_SESSION | Secret.ServiceFlags.LOAD_COLLECTIONS,
            cancel
        );

        /* Initial load before we arm listeners so we don't miss items
         * that were stored between process start and listener registration. */
        yield reload (cancel);

        /* Watch org.freedesktop.Secret.Service D-Bus signals so we hear about
         * collection changes from any source.  Secret.Service extends
         * GLib.DBusProxy, which exposes the g-signal GLib signal for this. */
        ((GLib.DBusProxy) _service).g_signal.connect (on_service_dbus_signal);

        /* Fast path: setup wizard emits these immediately after writing to the
         * keyring so the volume appears in Nautilus without a round-trip. */
        _bus = yield GLib.Bus.get (GLib.BusType.SESSION, cancel);

        _sub_added = _bus.signal_subscribe (
            null,
            "org.gnome.ProtonDrive",
            "AccountAdded",
            "/org/gnome/ProtonDrive",
            null,
            GLib.DBusSignalFlags.NONE,
            on_dbus_account_added
        );

        _sub_removed = _bus.signal_subscribe (
            null,
            "org.gnome.ProtonDrive",
            "AccountRemoved",
            "/org/gnome/ProtonDrive",
            null,
            GLib.DBusSignalFlags.NONE,
            on_dbus_account_removed
        );
    }

    public void stop () {
        if (_service != null)
            ((GLib.DBusProxy) _service).g_signal.disconnect (on_service_dbus_signal);
        if (_bus != null) {
            _bus.signal_unsubscribe (_sub_added);
            _bus.signal_unsubscribe (_sub_removed);
        }
    }

    /* ------------------------------------------------------------------ */
    /* Private                                                              */
    /* ------------------------------------------------------------------ */

    /**
     * Enumerate all uid entries (one per account) and diff against the known
     * set, emitting signals for any delta.
     */
    private async void reload (GLib.Cancellable? cancel = null) throws GLib.Error {
        var attrs = new GLib.HashTable<string, string> (str_hash, str_equal);
        attrs["field"] = "uid";

        var items = yield Secret.password_searchv (
            _schema,
            attrs,
            Secret.SearchFlags.ALL,
            cancel
        );

        var current = new GLib.HashTable<string, bool> (str_hash, str_equal);
        foreach (var item in items) {
            var item_attrs = item.get_attributes ();
            var username   = item_attrs["username"];
            if (username != null)
                current[username] = true;
        }

        /* Emit account_added for anything new. */
        current.foreach ((username, _) => {
            if (!_known.contains (username)) {
                _known[username] = true;
                account_added (username);
            }
        });

        /* Collect removals first to avoid modifying _known while iterating. */
        var removed = new GLib.List<string> ();
        _known.foreach ((username, _) => {
            if (!current.contains (username))
                removed.append (username);
        });
        foreach (var username in removed) {
            _known.remove (username);
            account_removed (username);
        }
    }

    private void on_service_dbus_signal (string? _sender,
                                          string  signal_name,
                                          GLib.Variant _params)
    {
        if (signal_name == "CollectionCreated" ||
            signal_name == "CollectionDeleted" ||
            signal_name == "CollectionChanged")
        {
            reload.begin (null, (obj, res) => {
                try {
                    reload.end (res);
                } catch (GLib.Error e) {
                    GLib.warning ("ProtonAccountWatcher: reload failed: %s", e.message);
                }
            });
        }
    }

    private void on_dbus_account_added (GLib.DBusConnection _conn,
                                         string?             _sender,
                                         string              _obj_path,
                                         string              _iface,
                                         string              _signal,
                                         GLib.Variant        params)
    {
        var username = params.get_child_value (0).get_string ();
        if (_known.contains (username))
            return;
        _known[username] = true;
        account_added (username);
    }

    private void on_dbus_account_removed (GLib.DBusConnection _conn,
                                           string?             _sender,
                                           string              _obj_path,
                                           string              _iface,
                                           string              _signal,
                                           GLib.Variant        params)
    {
        var username = params.get_child_value (0).get_string ();
        if (!_known.contains (username))
            return;
        _known.remove (username);
        account_removed (username);
    }
}
