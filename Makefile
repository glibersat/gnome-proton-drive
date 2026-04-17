# Makefile for gnome-proton
#
# Builds and installs four components:
#   1. proton-drive-helper          (Go, installed to libexecdir)
#   2. gvfsd-proton                 (C/GVfs backend, built with Meson)
#   3. proton-drive-setup           (Python GTK wizard, installed to bindir)
#   4. gvfsd-proton-volume-monitor  (Vala GVfs remote volume monitor, Meson)
#
# Usage:
#   make                 build all
#   make install         install all (may need sudo)
#   make test            run all test suites
#   make clean           remove build artefacts
#   make uninstall       remove installed files (may need sudo)

PREFIX     ?= /usr/local
DESTDIR    ?=
BINDIR     := $(DESTDIR)$(PREFIX)/bin
LIBEXECDIR := $(DESTDIR)$(PREFIX)/libexec

BUILDDIR         := _build
MONITOR_BUILDDIR := _build-monitor

HELPER_SRC  := helper
BACKEND_SRC := backend
MONITOR_SRC := volume-monitor

GO         ?= go
MESON      ?= meson
NINJA      ?= ninja

# ---------------------------------------------------------------------------
# Default target
# ---------------------------------------------------------------------------

.PHONY: all
all: build-helper build-backend build-monitor

# ---------------------------------------------------------------------------
# Go helper
# ---------------------------------------------------------------------------

HELPER_BIN := $(HELPER_SRC)/proton-drive-helper

.PHONY: build-helper
build-helper:
	$(GO) build -C $(HELPER_SRC) -o proton-drive-helper .

.PHONY: test-helper
test-helper:
	$(GO) test -C $(HELPER_SRC) -race ./...

# ---------------------------------------------------------------------------
# C backend (Meson)
# ---------------------------------------------------------------------------

$(BUILDDIR)/build.ninja:
	$(MESON) setup $(BUILDDIR) $(BACKEND_SRC) \
	  --prefix=$(PREFIX) \
	  --libexecdir=libexec \
	  -Doptimization=2

.PHONY: build-backend
build-backend: $(BUILDDIR)/build.ninja
	$(NINJA) -C $(BUILDDIR)

.PHONY: test-backend
test-backend: $(BUILDDIR)/build.ninja
	$(MESON) test -C $(BUILDDIR) --print-errorlogs

# ---------------------------------------------------------------------------
# Vala volume monitor (Meson)
# ---------------------------------------------------------------------------

$(MONITOR_BUILDDIR)/build.ninja:
	$(MESON) setup $(MONITOR_BUILDDIR) $(MONITOR_SRC) \
	  --prefix=$(PREFIX) \
	  --libexecdir=libexec \
	  -Doptimization=2

.PHONY: build-monitor
build-monitor: $(MONITOR_BUILDDIR)/build.ninja
	$(NINJA) -C $(MONITOR_BUILDDIR)

# ---------------------------------------------------------------------------
# Combined test target
# ---------------------------------------------------------------------------

.PHONY: test
test: test-helper test-backend

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------

.PHONY: install
install: build-helper build-backend build-monitor install-helper install-backend install-monitor install-setup

.PHONY: install-helper
install-helper:
	install -Dm755 $(HELPER_BIN) $(LIBEXECDIR)/proton-drive-helper

.PHONY: install-backend
install-backend:
	DESTDIR=$(DESTDIR) $(MESON) install -C $(BUILDDIR)

.PHONY: install-monitor
install-monitor:
	DESTDIR=$(DESTDIR) $(MESON) install -C $(MONITOR_BUILDDIR)

.PHONY: install-setup
install-setup:
	install -Dm755 proton-drive-setup $(BINDIR)/proton-drive-setup

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

.PHONY: uninstall
uninstall:
	rm -f $(LIBEXECDIR)/proton-drive-helper
	rm -f $(LIBEXECDIR)/gvfsd-proton-volume-monitor
	rm -f $(BINDIR)/proton-drive-setup
	@echo "Run 'meson install --destdir / --only-changed' or remove GVfs files manually."
	@echo "Typical paths:"
	@echo "  /usr/lib/gvfs/gvfsd-proton"
	@echo "  /usr/share/gvfs/mounts/proton.mount"
	@echo "  /usr/share/gvfs/remote-volume-monitors/proton.monitor"

# ---------------------------------------------------------------------------
# Clean
# ---------------------------------------------------------------------------

.PHONY: clean
clean:
	rm -f $(HELPER_BIN)
	rm -rf $(BUILDDIR)
	rm -rf $(MONITOR_BUILDDIR)
