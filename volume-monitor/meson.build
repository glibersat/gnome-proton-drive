project('proton-volume-monitor', ['vala', 'c'],
  version: '0.1.0',
  default_options: ['warning_level=2'],
)

glib_dep       = dependency('glib-2.0',       version: '>= 2.76')
gio_dep        = dependency('gio-2.0',         version: '>= 2.76')
gio_unix_dep   = dependency('gio-unix-2.0')
libsecret_dep  = dependency('libsecret-1',     version: '>= 0.20')

gvfs_dep         = dependency('gvfs', required: false)
gvfs_monitor_dir = get_option('gvfs-remote-volume-monitor-dir')
if gvfs_monitor_dir == ''
  gvfs_monitor_dir = gvfs_dep.found() \
    ? gvfs_dep.get_variable(pkgconfig: 'remotevolumemonitordir') \
    : '/usr/share/gvfs/remote-volume-monitors'
endif
message('GVfs remote volume monitor dir: ' + gvfs_monitor_dir)

executable(
  'gvfsd-proton-volume-monitor',
  [
    'main.vala',
    'proton-account-watcher.vala',
    'proton-volume.vala',
    'proton-remote-volume-monitor.vala',
  ],
  dependencies: [glib_dep, gio_dep, gio_unix_dep, libsecret_dep],
  install: true,
  install_dir: get_option('libexecdir'),
)

configure_file(
  input:  'proton.monitor.in',
  output: 'proton.monitor',
  configuration: {'libexecdir': get_option('prefix') / get_option('libexecdir')},
  install: true,
  install_dir: gvfs_monitor_dir,
)
