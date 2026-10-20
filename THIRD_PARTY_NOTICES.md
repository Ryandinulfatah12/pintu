# Third-party notices

Pintu itself is licensed under the [MIT License](LICENSE). It includes or links the following third-party software.

## Bundled in `public/vendor/`

| Component | Version | License | License file |
|---|---|---|---|
| [guacamole-common-js](https://github.com/apache/guacamole-client) (Apache Guacamole) | 1.5.0 | Apache-2.0 | [`public/vendor/guacamole-common.LICENSE`](public/vendor/guacamole-common.LICENSE) |
| [xterm.js](https://github.com/xtermjs/xterm.js) (`@xterm/xterm`) | 5.5.0 | MIT | [`public/vendor/xterm/LICENSE`](public/vendor/xterm/LICENSE) |
| `@xterm/addon-fit` | 0.10.0 | MIT | [`public/vendor/xterm/LICENSE`](public/vendor/xterm/LICENSE) |
| `@xterm/addon-webgl` | 0.18.0 | MIT | [`public/vendor/xterm/LICENSE`](public/vendor/xterm/LICENSE) |

guacamole-common-js carries the following NOTICE (from apache/guacamole-client 1.5.0), also shipped as [`public/vendor/guacamole-common.NOTICE`](public/vendor/guacamole-common.NOTICE):

```
Apache Guacamole
Copyright 2020 The Apache Software Foundation

This product includes software developed at
The Apache Software Foundation (https://www.apache.org/).
```

## Go modules compiled into the binary

The exact set depends on the target OS. The Docker image is a Linux build.

| Module | License | Linked on |
|---|---|---|
| [github.com/coder/websocket](https://github.com/coder/websocket) | ISC | all |
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) | BSD-3-Clause | all |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | BSD-3-Clause | all |
| [golang.org/x/exp](https://pkg.go.dev/golang.org/x/exp) | BSD-3-Clause | all |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | BSD-3-Clause | all |
| [modernc.org/libc](https://gitlab.com/cznic/libc) | BSD-3-Clause | all |
| [modernc.org/mathutil](https://gitlab.com/cznic/mathutil) | BSD-3-Clause | all |
| [modernc.org/memory](https://gitlab.com/cznic/memory) | BSD-3-Clause | all |
| [github.com/dustin/go-humanize](https://github.com/dustin/go-humanize) | MIT | all |
| [github.com/remyoudompheng/bigfft](https://github.com/remyoudompheng/bigfft) | BSD-3-Clause | all |
| [github.com/google/uuid](https://github.com/google/uuid) | BSD-3-Clause | Linux, macOS |
| [github.com/mattn/go-isatty](https://github.com/mattn/go-isatty) | MIT | Windows, macOS |
| [github.com/ncruces/go-strftime](https://github.com/ncruces/go-strftime) | MIT | Windows, macOS |

The full license texts are included with each module's source (see `go mod download -json` for their locations).

## Used at runtime, not bundled

- [Apache Guacamole `guacd`](https://guacamole.apache.org/) (Apache-2.0) runs as a separate container (`guacamole/guacd`) for RDP.
- [IBM Plex Sans and IBM Plex Mono](https://github.com/IBM/plex) (SIL Open Font License 1.1) are loaded from Google Fonts by the browser.
