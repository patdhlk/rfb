package main

import (
	"flag"
	"image"
	"log"
	"net"
	"os"
	"runtime/pprof"
	"time"

	"github.com/kbinani/screenshot"
	"github.com/patdhlk/rfb"
)

var (
	bindAddress = flag.String("bindAddress", "localhost:5900", "listen on [ip]:port")
	profile     = flag.Bool("profile", false, "write a cpu.prof file when client disconnects")
)

func main() {
	flag.Parse()

	ln, err := net.Listen("tcp", *bindAddress)
	if err != nil {
		log.Fatal(err)
	}

	if screens := screenshot.NumActiveDisplays(); screens < 1 {
		log.Fatal("no screens found!")
	} else if screens > 1 {
		log.Print("warning: more than one screen, only casting the first")
	}

	var rect = screenshot.GetDisplayBounds(0)
	s := rfb.NewServer(rect.Size().X, rect.Size().Y)
	go func() {
		err = s.Serve(ln)
		log.Fatalf("rfb server failed with: %v", err)
	}()
	for c := range s.Conns {
		handleConn(c)
	}
}

func handleConn(c *rfb.Conn) {
	if *profile {
		f, err := os.Create("cpu.prof")
		if err != nil {
			log.Fatal(err)
		}
		err = pprof.StartCPUProfile(f)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("profiling CPU")
		defer pprof.StopCPUProfile()
		defer log.Printf("stopping profiling CPU")
	}

	var rect = screenshot.GetDisplayBounds(0)
	log.Println("screen size: ", rect.Size().X, "x", rect.Size().Y)
	im := image.NewRGBA(rect)
	li := &rfb.LockableImage{Img: im}

	closec := make(chan bool)
	go func() {
		tick := time.NewTicker(time.Second / 30)
		defer tick.Stop()
		haveNewFrame := false

		//var ts = time.Now()
		for {
			feed := c.Feed
			if !haveNewFrame {
				feed = nil
			}
			_ = feed
			select {
			case feed <- li:
				haveNewFrame = false
			case <-closec:
				return
			case <-tick.C:
				li.Lock()
				//var img *image.RGBA
				var err error
				var img *image.RGBA
				if img, err = screenshot.CaptureDisplay(0); err != nil {
					log.Fatal(err)
				}

				//var now = time.Now()
				li.Img = img
				li.Unlock()
				haveNewFrame = true
				//log.Printf("dbg: %dms", now.Sub(ts)/time.Millisecond)
				//ts = now
			}
		}
	}()

	for e := range c.Event {
		switch e.(type) {
		case rfb.KeyEvent:
			var ke = e.(rfb.KeyEvent)
			log.Printf("got key event: %#v", ke)
		case rfb.PointerEvent:
			var me = e.(rfb.PointerEvent)
			log.Printf("got pointer event: %#v", me)
		default:
			log.Printf("got unsupported event: %#v", e)
		}
	}
	close(closec)
	log.Printf("Client disconnected")
}
