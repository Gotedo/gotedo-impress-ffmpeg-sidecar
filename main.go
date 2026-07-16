package main

/*
#include "decoder.h"
*/
import "C"
import (
	"unsafe"
)

func main() {
	var ctx C.DemuxDecContext
	path := C.CString("file.mkv")
	defer C.free(unsafe.Pointer(path))

	// We cleanly invoke our C function
	_ = C.open_input_and_decoders(&ctx, path)
	C.free_demux_dec_context(&ctx)
}
