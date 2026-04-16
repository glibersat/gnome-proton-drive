project('gvfsd-proton', 'c',
  version: '0.1.0',
  default_options: ['c_std=c11', 'warning_level=2'],
)

cc = meson.get_compiler('c')

glib_dep     = dependency('glib-2.0',    version: '>= 2.76')
gio_dep      = dependency('gio-2.0',     version: '>= 2.76')
gio_unix_dep = dependency('gio-unix-2.0')
json_glib_dep = dependency('json-glib-1.0')

# GVfs daemon private library — not installed with headers, so we vendor
# the headers from the matching source tree under gvfs-headers/.
gvfs_daemon_lib  = cc.find_library('gvfsdaemon',
  dirs: ['/usr/lib/x86_64-linux-gnu/gvfs'],
)
gvfs_common_lib  = cc.find_library('gvfscommon',
  dirs: ['/usr/lib/x86_64-linux-gnu/gvfs'],
)

gvfs_headers_inc = include_directories('gvfs-headers')
backend_inc      = include_directories('.')

gvfsd_proton = executable(
  'gvfsd-proton',
  [
    'gvfsbackendproton.c',
    'proton-rpc.c',
    'daemon-main.c',
    'daemon-main-generic.c',
  ],
  c_args: [
    '-DBACKEND_HEADER=gvfsbackendproton.h',
    '-DDEFAULT_BACKEND_TYPE=proton',
    '-DBACKEND_TYPES="proton", G_VFS_TYPE_BACKEND_PROTON,',
    '-DMAX_JOB_THREADS=10',
    '-DBACKEND_USES_GVFS',
    '-DPROTON_LIBEXECDIR="' + get_option('prefix') / get_option('libexecdir') + '"',
  ],
  include_directories: [backend_inc, gvfs_headers_inc],
  dependencies: [glib_dep, gio_dep, gio_unix_dep, json_glib_dep,
                 gvfs_daemon_lib, gvfs_common_lib],
  install: true,
  install_dir: get_option('libexecdir'),
)

# Mount descriptor — @libexecdir@ is substituted at install time
configure_file(
  input:  'proton.mount',
  output: 'proton.mount',
  configuration: {'libexecdir': get_option('prefix') / get_option('libexecdir')},
  install: true,
  install_dir: get_option('datadir') / 'gvfs' / 'mounts',
)

subdir('tests')
