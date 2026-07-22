package scan

// ANSI SGR sequences for colored output, following GNU grep's default
// palette: magenta filenames (here: object keys), green line numbers,
// cyan separators, bold red matched text. Colors are applied after
// sanitization, so escape sequences in scanned content can never be
// confused with ours.
const (
	ansiReset  = "\x1b[0m"
	ansiKey    = "\x1b[35m"   // object key: magenta
	ansiZip    = "\x1b[36m"   // zip entry name: cyan
	ansiLineNo = "\x1b[32m"   // line number: green
	ansiSep    = "\x1b[36m"   // separators: cyan
	ansiMatch  = "\x1b[1;31m" // matched text: bold red
)
