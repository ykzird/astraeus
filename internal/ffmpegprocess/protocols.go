// Package ffmpegprocess holds the settings every ffmpeg and ffprobe invocation in
// this server must share.
//
// It exists because the same mistake was made in four places independently: the
// tools were run with their default protocol set, which includes every network
// protocol they were built with. That matters because ffmpeg does not treat an
// input as "a file" - it treats it as a URL, and a file whose contents look like a
// playlist is interpreted as instructions to fetch other URLs. A library is a
// directory an operator pointed the server at, so anything that can write a file
// there could make the server issue requests wherever that file said (S-17 of the
// 2026-10-09 review).
//
// One constant in one place, so the next invocation is added with it rather than
// without it.
package ffmpegprocess

// LocalProtocols is the set of protocols a library file may use.
//
// Everything this server reads is a path on disk:
//
//   - `file` is reading that path;
//   - `pipe` is reading a descriptor, which the tools use internally for some
//     demuxers and which a test fixture may use;
//   - `data` covers an inline data URI, which a generated fixture can contain;
//   - `crypto` covers an encrypted segment a real release might carry.
//
// Nothing here reaches the network, which is the point: `http`, `https`, `tcp`,
// `udp`, `rtmp`, `rtsp`, `ftp` and the rest are absent, so a playlist that names
// one is refused rather than fetched.
const LocalProtocols = "file,pipe,data,crypto"

// Args returns the arguments that restrict an invocation's protocols.
//
// The option applies to the inputs that follow it, so callers place this before
// the first -i. Returning a slice rather than a pair of strings keeps callers from
// having to remember the flag's spelling or the constant's name.
func Args() []string {
	return []string{"-protocol_whitelist", LocalProtocols}
}
