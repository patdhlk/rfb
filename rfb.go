package rfb

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"image"
	"log"
	"net"
	"strconv"
	"sync"
)

const (
	v3 = "RFB 003.003\n"
	v7 = "RFB 003.007\n"
	v8 = "RFB 003.008\n"

	authNone = 1

	statusOK     = 0
	statusFailed = 1

	encodingRaw = 0
	//encodingCopyRect = 1

	// Client -> Server
	cmdSetPixelFormat           = 0
	cmdSetEncodings             = 2
	cmdFramebufferUpdateRequest = 3
	cmdKeyEvent                 = 4
	cmdPointerEvent             = 5
	cmdClientCutText            = 6

	// Server -> Client
	cmdFramebufferUpdate = 0
)

func NewServer(width, height int) *Server {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	conns := make(chan *Conn, 16)
	return &Server{
		width:  width,
		height: height,
		Conns:  conns,
		conns:  conns,
	}
}

type Server struct {
	width, height int
	conns         chan *Conn // read/write version of Conns

	// Conns is a channel of incoming connections.
	Conns <-chan *Conn
}

func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		conn := s.newConn(c)
		select {
		case s.conns <- conn:
		default:
			// client is behind; doesn't get this updated.
		}
		go conn.serve()
	}
}

func (s *Server) newConn(c net.Conn) *Conn {
	feed := make(chan *LockableImage, 16)
	event := make(chan interface{}, 16)
	conn := &Conn{
		s:      s,
		c:      c,
		br:     bufio.NewReader(c),
		bw:     bufio.NewWriter(c),
		fbupc:  make(chan FrameBufferUpdateRequest, 128),
		closec: make(chan bool),
		feed:   feed,
		Feed:   feed, // the send-only version
		event:  event,
		Event:  event, // the recieve-only version
	}
	return conn
}

type LockableImage struct {
	sync.RWMutex
	Img image.Image
}

type Conn struct {
	s      *Server
	c      net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	fbupc  chan FrameBufferUpdateRequest
	closec chan bool // never sent; just closed

	// should only be mutated once during handshake, but then
	// only read.
	format PixelFormat

	feed chan *LockableImage
	mu   sync.RWMutex // guards last
	last image.Image  // pointer to read only image (the last we've sent to the client)

	buf8 []uint8 // temporary buffer to avoid generating garbage

	// Feed is the channel to send new frames.
	Feed chan<- *LockableImage

	// Event is a readable channel of events from the client.
	// The value will be either a KeyEvent or PointerEvent.  The
	// channel is closed when the client disconnects.
	Event <-chan interface{}

	event chan interface{} // internal version of Event

	gotFirstFrame bool
}

func (c *Conn) dimensions() (w, h int) {
	return c.s.width, c.s.height
}

func (c *Conn) readByte(what string) byte {
	b, err := c.br.ReadByte()
	if err != nil {
		c.failf("reading client byte for %q: %v", what, err)
	}
	return b
}

func (c *Conn) readPadding(what string, size int) {
	for i := 0; i < size; i++ {
		c.readByte(what)
	}
}

func (c *Conn) read(what string, v interface{}) {
	err := binary.Read(c.br, binary.BigEndian, v)
	if err != nil {
		c.failf("reading from client into %T for %q: %v", v, what, err)
	}
}

func (c *Conn) w(v interface{}) {
	binary.Write(c.bw, binary.BigEndian, v)
}

func (c *Conn) flush() {
	c.bw.Flush()
}

func (c *Conn) failf(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}

func (c *Conn) serve() {
	defer c.c.Close()
	defer close(c.fbupc)
	defer close(c.closec)
	defer close(c.event)
	defer func() {
		e := recover()
		if e != nil {
			log.Printf("Client disconnect: %v", e)
		}
	}()

	c.bw.WriteString("RFB 003.008\n")
	c.flush()
	sl, err := c.br.ReadSlice('\n')
	if err != nil {
		c.failf("reading client protocol version: %v", err)
	}
	ver := string(sl)
	log.Printf("client wants: %q", ver)
	switch ver {
	case v3, v7, v8: // cool.
	default:
		c.failf("bogus client-requested security type %q", ver)
	}

	// Auth
	if ver >= v7 {
		// Just 1 auth type supported: 1 (no auth)
		c.bw.WriteString("\x01\x01")
		c.flush()
		wanted := c.readByte("6.1.2:client requested security-type")
		if wanted != authNone {
			c.failf("client wanted auth type %d, not None", int(wanted))
		}
	} else {
		// Old way. Just tell client we're doing no auth.
		c.w(uint32(authNone))
		c.flush()
	}

	if ver >= v8 {
		// 6.1.3. SecurityResult
		c.w(uint32(statusOK))
		c.flush()
	}

	log.Printf("reading client init")

	// ClientInit
	wantShared := c.readByte("shared-flag") != 0
	_ = wantShared

	c.format = PixelFormat{
		BPP:        16,
		Depth:      16,
		BigEndian:  0,
		TrueColour: 1,
		RedMax:     0x1f,
		GreenMax:   0x1f,
		BlueMax:    0x1f,
		RedShift:   0xa,
		GreenShift: 0x5,
		BlueShift:  0,
	}

	// 6.3.2. ServerInit
	width, height := c.dimensions()
	c.w(uint16(width))
	c.w(uint16(height))
	c.w(c.format.BPP)
	c.w(c.format.Depth)
	c.w(c.format.BigEndian)
	c.w(c.format.TrueColour)
	c.w(c.format.RedMax)
	c.w(c.format.GreenMax)
	c.w(c.format.BlueMax)
	c.w(c.format.RedShift)
	c.w(c.format.GreenShift)
	c.w(c.format.BlueShift)
	c.w(uint8(0)) // pad1
	c.w(uint8(0)) // pad2
	c.w(uint8(0)) // pad3
	serverName := "rfb-go"
	c.w(int32(len(serverName)))
	c.bw.WriteString(serverName)
	c.flush()

	for {
		//log.Printf("awaiting command byte from client...")
		cmd := c.readByte("6.4:client-server-packet-type")
		//log.Printf("got command type %d from client", int(cmd))
		switch cmd {
		case cmdSetPixelFormat:
			c.handleSetPixelFormat()
		case cmdSetEncodings:
			c.handleSetEncodings()
		case cmdFramebufferUpdateRequest:
			c.handleUpdateRequest()
		case cmdPointerEvent:
			c.handlePointerEvent()
		case cmdKeyEvent:
			c.handleKeyEvent()
		default:
			c.failf("unsupported command type %d from client", int(cmd))
		}
	}
}

func (c *Conn) pushFramesLoop() {
	for {
		select {
		case ur, ok := <-c.fbupc:
			if !ok {
				// Client disconnected.
				return
			}
			c.pushFrame(ur)
		}
	}
}

func (c *Conn) pushFrame(ur FrameBufferUpdateRequest) {
	li := <-c.feed
	if li == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.pushImage(li, ur)
}

func (c *Conn) pushImage(li *LockableImage, ur FrameBufferUpdateRequest) {
	li.Lock()
	defer li.Unlock()

	var lastImg = c.last

	var rects []image.Rectangle
	if ur.incremental() {
		rects = compareImages(li.Img, lastImg)
	} else {
		rects = append(rects, li.Img.Bounds())
	}

	c.w(uint8(cmdFramebufferUpdate))
	c.w(uint8(0))           // padding byte
	c.w(uint16(len(rects))) // number of rectangles

	//log.Printf("sending %d changed sections", len(rects))

	if c.format.TrueColour == 0 {
		c.failf("only true-colour supported")
	}

	// Send rectangles:
	for _, rect := range rects {
		c.w(uint16(rect.Min.X)) // x
		c.w(uint16(rect.Min.Y)) // y
		c.w(uint16(rect.Dx()))  // width
		c.w(uint16(rect.Dy()))  // height
		c.w(int32(encodingRaw))

		// note: this doesn't work right now (pushRGBAScreensThousandsLocked() directly accesses the pixel buffer, ignoring the SubImage() boundaries)
		/*rgba, isRGBA := li.Img.(*image.RGBA)
		if isRGBA && c.format.isScreensThousands() {
			// Fast path.
			rgba = rgba.SubImage(rect).(*image.RGBA)
			c.pushRGBAScreensThousandsLocked(rgba)
		} else {*/
		c.pushGenericLocked(li.Img, rect)
		//}
	}
	c.flush()

	c.last = li.Img
}

func (c *Conn) pushRGBAScreensThousandsLocked(im *image.RGBA) {
	var u16 uint16
	pixels := len(im.Pix) / 4
	if len(c.buf8) < pixels*2 {
		c.buf8 = make([]byte, pixels*2)
	}
	out := c.buf8[:]
	isBigEndian := c.format.BigEndian != 0
	for i, v8 := range im.Pix {
		switch i % 4 {
		case 0: // red
			u16 = uint16(v8&248) << 7 // 3 masked bits + 7 shifted == redshift of 10
		case 1: // green
			u16 |= uint16(v8&248) << 2 // redshift of 5
		case 2: // blue
			u16 |= uint16(v8 >> 3)
		case 3: // alpha, unused.  use this to just move the dest
			hb, lb := uint8(u16>>8), uint8(u16)
			if isBigEndian {
				out[0] = hb
				out[1] = lb
			} else {
				out[0] = lb
				out[1] = hb
			}
			out = out[2:]
		}
	}
	c.bw.Write(c.buf8[:pixels*2])
}

// pushGenericLocked is the slow path generic implementation that works on
// any image.Image concrete type and any client-requested pixel format.
// If you're lucky, you never end in this path.
func (c *Conn) pushGenericLocked(im image.Image, rect image.Rectangle) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			col := im.At(x, y)
			r16, g16, b16, _ := col.RGBA()
			r16 = inRange(r16, c.format.RedMax)
			g16 = inRange(g16, c.format.GreenMax)
			b16 = inRange(b16, c.format.BlueMax)
			var u32 uint32 = (r16 << c.format.RedShift) |
				(g16 << c.format.GreenShift) |
				(b16 << c.format.BlueShift)
			var v interface{}
			switch c.format.BPP {
			case 32:
				v = u32
			case 16:
				v = uint16(u32)
			case 8:
				v = uint8(u32)
			default:
				c.failf("TODO: BPP of %d", c.format.BPP)
			}
			if c.format.BigEndian != 0 {
				binary.Write(c.bw, binary.BigEndian, v)
			} else {
				binary.Write(c.bw, binary.LittleEndian, v)
			}
		}
	}
}

type PixelFormat struct {
	BPP, Depth                      uint8
	BigEndian, TrueColour           uint8 // flags; 0 or non-zero
	RedMax, GreenMax, BlueMax       uint16
	RedShift, GreenShift, BlueShift uint8
}

// Is the format requested by the OS X "Screens" app's "Thousands" mode.
func (f *PixelFormat) isScreensThousands() bool {
	// Note: Screens asks for Depth 16; RealVNC asks for Depth 15 (which is more accurate)
	// Accept either. Same format.
	return f.BPP == 16 && (f.Depth == 16 || f.Depth == 15) && f.TrueColour != 0 &&
		f.RedMax == 0x1f && f.GreenMax == 0x1f && f.BlueMax == 0x1f &&
		f.RedShift == 10 && f.GreenShift == 5 && f.BlueShift == 0
}

// 6.4.1
func (c *Conn) handleSetPixelFormat() {
	log.Printf("handling setpixel format")
	c.readPadding("SetPixelFormat padding", 3)
	var pf PixelFormat
	c.read("pixelformat.bpp", &pf.BPP)
	c.read("pixelformat.depth", &pf.Depth)
	c.read("pixelformat.beflag", &pf.BigEndian)
	c.read("pixelformat.truecolour", &pf.TrueColour)
	c.read("pixelformat.redmax", &pf.RedMax)
	c.read("pixelformat.greenmax", &pf.GreenMax)
	c.read("pixelformat.bluemax", &pf.BlueMax)
	c.read("pixelformat.redshift", &pf.RedShift)
	c.read("pixelformat.greenshift", &pf.GreenShift)
	c.read("pixelformat.blueshift", &pf.BlueShift)
	c.readPadding("SetPixelFormat pixel format padding", 3)
	log.Printf("Client wants pixel format: %#v", pf)
	c.format = pf

	// TODO: send PixelFormat event? would clients care?
}

// 6.4.2
func (c *Conn) handleSetEncodings() {
	c.readPadding("SetEncodings padding", 1)

	var numEncodings uint16
	c.read("6.4.2:number-of-encodings", &numEncodings)
	var encType []int32
	for i := 0; i < int(numEncodings); i++ {
		var t int32
		c.read("encoding-type", &t)
		encType = append(encType, t)
	}
	log.Printf("Client encodings: %#v", encType)

}

// 6.4.3
type FrameBufferUpdateRequest struct {
	IncrementalFlag     uint8
	X, Y, Width, Height uint16
}

func (r *FrameBufferUpdateRequest) incremental() bool { return r.IncrementalFlag != 0 }

// 6.4.3
func (c *Conn) handleUpdateRequest() {
	if !c.gotFirstFrame {
		c.gotFirstFrame = true
		go c.pushFramesLoop()
	}

	var req FrameBufferUpdateRequest
	c.read("framebuffer-update.incremental", &req.IncrementalFlag)
	c.read("framebuffer-update.x", &req.X)
	c.read("framebuffer-update.y", &req.Y)
	c.read("framebuffer-update.width", &req.Width)
	c.read("framebuffer-update.height", &req.Height)
	c.fbupc <- req
}

// 6.4.4
type KeyEvent struct {
	DownFlag uint8
	Key      uint32
}

// 6.4.4
func (c *Conn) handleKeyEvent() {
	var req KeyEvent
	c.read("key-event.downflag", &req.DownFlag)
	c.readPadding("key-event.padding", 2)
	c.read("key-event.key", &req.Key)
	select {
	case c.event <- req:
	default:
		// Client's too slow.
	}
}

// 6.4.5
type PointerEvent struct {
	ButtonMask uint8
	X, Y       uint16
}

// 6.4.5
func (c *Conn) handlePointerEvent() {
	var req PointerEvent
	c.read("pointer-event.mask", &req.ButtonMask)
	c.read("pointer-event.x", &req.X)
	c.read("pointer-event.y", &req.Y)
	select {
	case c.event <- req:
	default:
		// Client's too slow.
	}
}

// compareImages -- chops the images in 64x64 squares and returns a list of changed sections
//
// note: this will only work if the application sends us references to different Image objects
// each time
func compareImages(oldImg image.Image, newImg image.Image) []image.Rectangle {
	var rc []image.Rectangle

	// prechecks
	if oldImg == nil && newImg == nil {
		panic("can't compare two nil images")
	} else if oldImg == nil {
		// first frame -> everything's changed
		rc = append(rc, newImg.Bounds())
		return rc
	} else if newImg == nil {
		// by pushing a nil image, the app code's telling us there have been no changes -> return empty list
		return []image.Rectangle{}
	} else if newImg.Bounds() != oldImg.Bounds() {
		panic("images have different sizes")
	}

	var minInt = func(a, b int) int { // helper function
		if a < b {
			return a
		}
		return b
	}

	const sectionSize = 64
	bounds := newImg.Bounds()
	for sectionTop := bounds.Min.Y; sectionTop < bounds.Max.Y; sectionTop += sectionSize {
		var changedSections = map[int]struct{}{} // x coordinates (sectionLeft) of the sections already in rc

		for y := sectionTop; y < bounds.Max.Y; y++ { // row by row
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				if oldImg.At(x, y) != newImg.At(x, y) {
					var sectionLeft = x - (x % sectionSize)
					if _, ok := changedSections[sectionLeft]; ok {
						continue
					}

					// add changed section to rc
					var sectionRight = minInt(sectionLeft+sectionSize, bounds.Max.X)
					var sectionBottom = minInt(sectionTop+sectionSize, bounds.Max.Y)
					rc = append(rc, image.Rect(sectionLeft, sectionTop, sectionRight, sectionBottom))
					changedSections[sectionLeft] = struct{}{}
				}
			}
		}
	}

	return rc
}

func inRange(v uint32, max uint16) uint32 {
	switch max {
	case 0x1f: // 5 bits
		return v >> (16 - 5)
	case 0x03: // 2 bits
		return v >> (16 - 2)
	}
	panic("todo; max value = " + strconv.Itoa(int(max)))
}
