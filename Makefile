BIN := sinty-diskd
PREFIX ?= /usr

.PHONY: all build static test vet install clean

all: build

build:
	go build -o $(BIN) ./cmd/sinty-diskd

static:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/sinty-diskd

test:
	go test ./...

vet:
	go vet ./...

install: static
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/libexec/$(BIN)
	install -Dm644 packaging/sinty-diskd.service $(DESTDIR)$(PREFIX)/lib/systemd/system/sinty-diskd.service
	install -Dm644 packaging/sinty-diskd.conf $(DESTDIR)$(PREFIX)/share/dbus-1/system.d/sinty-diskd.conf
	install -Dm644 packaging/org.freedesktop.UDisks2.service $(DESTDIR)$(PREFIX)/share/dbus-1/system-services/org.freedesktop.UDisks2.service

clean:
	rm -f $(BIN)
