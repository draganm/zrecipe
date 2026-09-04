// This directory is a copy of the flate package of
// github.com/klauspost/compress at v1.11.3 (2020-11-15), the generation
// that klauspost/pgzip compresses with in umoci and rockcraft. It is kept
// here, under zrecipe's own import path, because the pgzip engine must
// reproduce that generation's bytes while the rest of zrecipe links the
// module's current version, and Go cannot load two versions of one module.
//
// The files are upstream's non-test sources (the two go:generate programs,
// gen.go and gen_inflate.go, are left out) reformatted by gofmt; nothing
// else changed. They stay under upstream's BSD license, see LICENSE. Do not
// edit them: the engine's version string names this release, and a copy
// that compresses differently would no longer reproduce pgzip's files.

package flate
