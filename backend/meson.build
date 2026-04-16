project('gvfsd-proton', 'c',
  version: '0.1.0',
  default_options: ['c_std=c11', 'warning_level=2'],
)

cc = meson.get_compiler('c')

glib_dep     = dependency('glib-2.0',    version: '>= 2.76')
gio_dep      = dependency('gio-2.0',     version: '>= 2.76')
gio_unix_dep = dependency('gio-unix-2.0')
json_glib_dep = dependency('json-glib-1.0')

# Ask GVfs where it keeps its backends and mount descriptors.
# These are set at GVfs build time and may differ from our own prefix.
gvfs_dep       = dependency('gvfs', required: false)
gvfs_backend_dir = get_option('gvfs-backend-dir')
gvfs_mount_dir   = get_option('gvfs-mount-dir')
if gvfs_backend_dir == ''
  gvfs_backend_dir = gvfs_dep.found() \
    ? gvfs_dep.get_variable(pkgconfig: 'gvfsbackenddir') \
    : '/usr/lib/gvfs'
endif
if gvfs_mount_dir == ''
  gvfs_mount_dir = gvfs_dep.found() \
    ? gvfs_dep.get_variable(pkgconfig: 'gvfsmountdir') \
    : '/usr/share/gvfs/mounts'
endif
message('GVfs backend dir : ' + gvfs_backend_dir)
message('GVfs mount dir   : ' + gvfs_mount_dir)

# GVfs daemon private library — not installed with headers, so we vendor
# the headers from the matching source tree under gvfs-headers/.
# The library dir is not in the default ld.so path; we bake it as RPATH.
gvfs_lib_dir = get_option('gvfs-lib-dir')
if gvfs_lib_dir == ''
  # pkg-config does not expose the private lib dir; search common locations.
  _multiarch = run_command('dpkg-architecture', '-qDEB_HOST_MULTIARCH',
                           check: false).stdout().strip()
  _candidates = [
    gvfs_backend_dir,
    '/usr/lib/' + _multiarch + '/gvfs',
    '/usr/lib64/gvfs',
    '/usr/lib/gvfs',
  ]
  foreach _d : _candidates
    if gvfs_lib_dir == '' and cc.find_library('gvfsdaemon', dirs: [_d], required: false).found()
      gvfs_lib_dir = _d
    endif
  endforeach
endif
if gvfs_lib_dir == ''
  error('Could not find libgvfsdaemon.so — set -Dgvfs-lib-dir=<path>')
endif
message('GVfs lib dir     : ' + gvfs_lib_dir)

gvfs_daemon_lib  = cc.find_library('gvfsdaemon', dirs: [gvfs_lib_dir])
gvfs_common_lib  = cc.find_library('gvfscommon',  dirs: [gvfs_lib_dir])

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
  install_dir: gvfs_backend_dir,
  install_rpath: gvfs_lib_dir,
)

# Mount descriptor — Exec path points into the GVfs backend dir
configure_file(
  input:  'proton.mount',
  output: 'proton.mount',
  configuration: {'libexecdir': gvfs_backend_dir},
  install: true,
  install_dir: gvfs_mount_dir,
)

# Restart the GVfs daemon after install so it picks up the new backend.
meson.add_install_script('sh', '-c',
  'gvfsd --replace &')

subdir('tests')
