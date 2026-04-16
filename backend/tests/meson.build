test_proton_rpc = executable(
  'test-proton-rpc',
  ['test-proton-rpc.c', '../proton-rpc.c'],
  include_directories: [backend_inc, gvfs_headers_inc],
  dependencies: [glib_dep, gio_dep, gio_unix_dep, json_glib_dep],
)

test('proton-rpc', test_proton_rpc)
