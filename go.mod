module github.com/ykzird/astraeus

go 1.26.3

// The toolchain line is what keeps the released binary off a Go with known
// stdlib vulnerabilities. It is deliberately the newest 1.26 patch rather than
// the minimum this module needs: `go` stays at the language version the code is
// written against, while every build - CI, release and the Dockerfile builder -
// uses this toolchain, and `toolchain` outranks whatever Go the runner happens
// to have installed. Dependabot raises dependencies, not this line, so it is
// bumped by hand when a patch releases; govulncheck in CI is what makes a
// forgotten bump fail loudly.
toolchain go1.26.9

require (
	github.com/google/uuid v1.6.0
	github.com/jmoiron/sqlx v1.4.0
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
