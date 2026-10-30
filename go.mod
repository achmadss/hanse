module github.com/achmadss/hanse

go 1.22

// The toolchain line keeps the language level at 1.22 while vet, test,
// and lint build with Go 1.24, whose linker output macOS 26 dyld accepts;
// go1.22 test binaries abort there with "missing LC_UUID load command".
toolchain go1.24.0

require gopkg.in/yaml.v3 v3.0.1 // indirect
