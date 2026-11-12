# Community main test server MQTT scope

These scripts reconcile HA and HTTP device grants on the existing main47 test server. The HA and HTTP systemd timers run the corresponding script as scope.py in their runtime directory. Private MQTT credentials and active service configuration reside on the server.

When Broker Owner resolves the platform publisher scope dynamically, the timers leave its static allowlists untouched and omit publisher-only fields from new plugin credentials. The public installer does not invoke these server-specific scripts.
