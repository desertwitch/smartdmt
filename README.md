## smartdmt - SMART Device Monitoring Terminal

`smartdmt` is a small utility that provides a simple terminal user interface
(TUI) for inspecting your connected block devices' SMART information. It was
developed primarily as a companion application to
[ShredOS](https://github.com/PartialVolume/shredos.x86_64), so that such
information can be observed during disk wiping. However, the utility also
functions as a standalone program.

The TUI acts as a visual wrapper, calling `lsblk` and `smartctl` under the hood
to provide a side-by-side view of all block devices and their SMART information.
The data is automatically refreshed and basic filtering is possible.

### Installation

To build from source, a `Makefile` is included with the project's source code.
Running `make all` will compile the application and pull in any necessary
dependencies. `make check` runs the test suite and static analysis tools.

#### Runtime dependencies:
- `lsblk` (as part of `util-linux` package)
- `smartctl` (as part of `smartmontools` package)

#### Building from source:
```
git clone https://github.com/desertwitch/smartdmt.git
cd smartdmt
make all
```

## License

All code is licensed under the MIT License.
