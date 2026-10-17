# Contributing to sinty-diskd

Thanks for your interest in contributing!

## Quick Start

```bash
git clone https://github.com/singularityos-lab/sinty-diskd
cd sinty-diskd
go build ./...
go test ./...
```

## Guidelines

- Keep changes additive and tested. sinty-diskd runs as root and mounts
  filesystems; prefer small, verifiable commits over large rewrites.
- What UDisks2 clients see must not change: run the comparison test against
  a capture from a real machine before changing a value.
- Match the surrounding style. Comments explain WHY, not WHAT.
- By submitting a contribution you agree to the [CLA](CLA.md).

## License

GPL-3.0-only.
