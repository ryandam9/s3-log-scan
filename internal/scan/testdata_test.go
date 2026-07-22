package scan

// bz2Sample is `printf 'ERROR bz2 works\n' | bzip2 -c`, embedded
// because compress/bzip2 provides no writer.
var bz2Sample = []byte{
	66, 90, 104, 57, 49, 65, 89, 38, 83, 89, 14, 248, 66, 81, 0, 0,
	2, 95, 128, 0, 16, 64, 0, 16, 0, 2, 0, 144, 0, 16, 8, 152,
	144, 32, 0, 49, 0, 208, 1, 65, 233, 61, 38, 122, 165, 117, 11, 143,
	77, 216, 8, 248, 187, 146, 41, 194, 132, 128, 119, 194, 18, 136,
}
