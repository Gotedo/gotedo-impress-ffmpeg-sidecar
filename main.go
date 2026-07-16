package main

/*
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
*/
import "C"
import (
	"fmt"
	"log"
)

func main() {
	fmt.Println("Gotedo FFmpeg Sidecar (Static Build)")

	codecVer := C.avcodec_version()
	formatVer := C.avformat_version()
	utilVer := C.avutil_version()

	log.Printf("libavcodec version: %d", codecVer)
	log.Printf("libavformat version: %d", formatVer)
	log.Printf("libavutil version:  %d", utilVer)

	configStr := C.GoString(C.avcodec_configuration())
	fmt.Printf("\nFFmpeg Build Configuration:\n%s\n", configStr)

	fmt.Println("\nSuccess: FFmpeg statically linked into a single binary!")
}
