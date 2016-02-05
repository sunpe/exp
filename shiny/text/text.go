// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package text lays out paragraphs of text.
//
// A body of text is laid out into a Frame: Frames contain Paragraphs (stacked
// vertically), Paragraphs contain Lines (stacked vertically), and Lines
// contain Boxes (stacked horizontally). Each Box holds a []byte slice of the
// text. For example, to simply print a Frame's text from start to finish:
//
//	var f *text.Frame = etc
//	for p := f.FirstParagraph(); p != nil; p = p.Next(f) {
//		for l := p.FirstLine(f); l != nil; l = l.Next(f) {
//			for b := l.FirstBox(f); b != nil; b = b.Next(f) {
//				fmt.Print(b.Text(f))
//			}
//		}
//	}
//
// A Frame's structure (the tree of Paragraphs, Lines and Boxes), and its
// []byte text, are not modified directly. Instead, a Frame's maximum width can
// be re-sized, and text can be added and removed via Carets (which implement
// standard io interfaces). For example, to add some words to the end of a
// frame:
//
//	var f *text.Frame = etc
//	c := f.NewCaret()
//	c.Seek(0, text.SeekEnd)
//	c.WriteString("Not with a bang but a whimper.\n")
//	c.Close()
//
// Either way, such modifications can cause re-layout, which can add or remove
// Paragraphs, Lines and Boxes. The underlying memory for such structs can be
// re-used, so pointer values, such as of type *Box, should not be held over
// such modifications.
package text // import "golang.org/x/exp/shiny/text"

// TODO: for the various Frame and Caret methods, be principled about whether
// callee or caller is responsible for fixing up other Carets.

import (
	"io"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// These constants are equal to os.SEEK_SET, os.SEEK_CUR and os.SEEK_END,
// understood by the io.Seeker interface, and are provided so that users of
// this package don't have to explicitly import "os".
const (
	SeekSet int = 0
	SeekCur int = 1
	SeekEnd int = 2
)

// maxLen is maximum (inclusive) value of len(Frame.text) and therefore of
// Box.i, Box.j and Caret.k.
const maxLen = 0x7fffffff

// Frame holds Paragraphs of text.
//
// The zero value is a valid Frame of empty text, which contains one Paragraph,
// which contains one Line, which contains one Box.
type Frame struct {
	// These slices hold the Frame's Paragraphs, Lines and Boxes, indexed by
	// fields such as Paragraph.firstL and Box.next.
	//
	// Their contents are not necessarily in layout order. Each slice is
	// obviously backed by an array, but a Frame's list of children
	// (Paragraphs) forms a doubly-linked list, not an array list, so that
	// insertion has lower algorithmic complexity. Similarly for a Paragraph's
	// list of children (Lines) and a Line's list of children (Boxes).
	//
	// The 0'th index into each slice is a special case. Otherwise, each
	// element is either in use (forming a double linked list with its
	// siblings) or in a free list (forming a single linked list; the prev
	// field is -1).
	//
	// A zero firstFoo field means that the parent holds a single, implicit
	// (lazily allocated), empty-but-not-nil *Foo child. Every Frame contains
	// at least one Paragraph. Similarly, every Paragraph contains at least one
	// Line, and every Line contains at least one Box.
	//
	// A zero next or prev field means that there is no such sibling (for an
	// in-use Paragraph, Line or Box) or no such next free element (if in the
	// free list).
	paragraphs []Paragraph
	lines      []Line
	boxes      []Box

	// freeX is the index of the first X (Paragraph, Line or Box) in the
	// respective free list. Zero means that there is no such free element.
	freeP, freeL, freeB int32

	firstP int32

	maxWidth fixed.Int26_6
	face     font.Face

	// len is the total length of the Frame's current textual content, in
	// bytes. It can be smaller then len(text), since that []byte can contain
	// 'holes' of deleted content.
	//
	// Like the paragraphs, lines and boxes slice-typed fields above, the text
	// []byte does not necessarily hold the textual content in layout order.
	// Instead, it holds the content in edit (insertion) order, with occasional
	// compactions. Again, the algorithmic complexity of insertions matters.
	len  int
	text []byte

	carets []*Caret

	// lineReaderData supports the Frame's lineReader, used when reading a
	// Line's text one rune at a time.
	lineReaderData bAndK
}

// TODO: allow multiple font faces, i.e. rich text?

// SetFace sets the font face for measuring text.
func (f *Frame) SetFace(face font.Face) {
	f.face = face
	if f.len != 0 {
		f.relayout()
	}
}

// SetMaxWidth sets the target maximum width of a Line of text. Text will be
// broken so that a Line's width is less than or equal to this maximum width.
// This line breaking is not strict. A Line containing asingleverylongword
// combined with a narrow maximum width will not be broken and will remain
// longer than the target maximum width; soft hyphens are not inserted.
//
// A non-positive argument is treated as an infinite maximum width.
func (f *Frame) SetMaxWidth(m fixed.Int26_6) {
	if f.maxWidth == m {
		return
	}
	f.maxWidth = m
	if f.len != 0 {
		f.relayout()
	}
}

func (f *Frame) relayout() {
	for p := f.firstP; p != 0; p = f.paragraphs[p].next {
		f.relayoutParagraph(p)
	}
	// TODO: update Carets' p, l, b, k fields?
}

func (f *Frame) relayoutParagraph(p int32) {
	// Merge all of pp's Lines into a single Line (the first Line) then layout
	// that Line.
	firstL := f.paragraphs[p].firstL
	ll := &f.lines[firstL]
	b := ll.firstB
	for ll.next != 0 {
		// Move b to the last Box in ll.
		for {
			if next := f.boxes[b].next; next != 0 {
				b = next
				continue
			}
			break
		}

		// Join that last Box with the first Box of ll's next Line.
		nextLL := &f.lines[ll.next]
		f.boxes[b].next = nextLL.firstB
		f.boxes[nextLL.firstB].prev = b
		f.joinBoxes(b, nextLL.firstB)
		toFree := ll.next
		ll.next = nextLL.next
		f.freeLine(toFree)
	}
	layout(f, firstL)
}

// joinBoxes merges two adjacent Boxes if the Box.j field of the first one
// equals the Box.i field of the second.
func (f *Frame) joinBoxes(b0, b1 int32) {
	bb0 := &f.boxes[b0]
	bb1 := &f.boxes[b1]
	if bb0.j != bb1.i {
		return
	}
	bb0.j = bb1.j
	bb0.next = bb1.next
	if bb0.next != 0 {
		f.boxes[bb0.next].prev = b0
	}
	f.freeBox(b1)
}

func (f *Frame) newParagraph() int32 {
	if f.freeP != 0 {
		p := f.freeP
		pp := &f.paragraphs[p]
		f.freeP = pp.next
		*pp = Paragraph{}
		return p
	}
	if len(f.paragraphs) == 0 {
		// The 1 is because the 0'th index is a special case.
		f.paragraphs = make([]Paragraph, 1, 16)
	}
	f.paragraphs = append(f.paragraphs, Paragraph{})
	return int32(len(f.paragraphs) - 1)
}

func (f *Frame) newLine() int32 {
	if f.freeL != 0 {
		l := f.freeL
		ll := &f.lines[l]
		f.freeL = ll.next
		*ll = Line{}
		return l
	}
	if len(f.lines) == 0 {
		// The 1 is because the 0'th index is a special case.
		f.lines = make([]Line, 1, 16)
	}
	f.lines = append(f.lines, Line{})
	return int32(len(f.lines) - 1)
}

func (f *Frame) newBox() int32 {
	if f.freeB != 0 {
		b := f.freeB
		bb := &f.boxes[b]
		f.freeB = bb.next
		*bb = Box{}
		return b
	}
	if len(f.boxes) == 0 {
		// The 1 is because the 0'th index is a special case.
		f.boxes = make([]Box, 1, 16)
	}
	f.boxes = append(f.boxes, Box{})
	return int32(len(f.boxes) - 1)
}

func (f *Frame) freeParagraph(p int32) {
	f.paragraphs[p] = Paragraph{next: f.freeP, prev: -1}
	f.freeP = p
	// TODO: run a compaction if the free-list is too large?
}

func (f *Frame) freeLine(l int32) {
	f.lines[l] = Line{next: f.freeL, prev: -1}
	f.freeL = l
	// TODO: run a compaction if the free-list is too large?
}

func (f *Frame) freeBox(b int32) {
	f.boxes[b] = Box{next: f.freeB, prev: -1}
	f.freeB = b
	// TODO: run a compaction if the free-list is too large?
}

func (f *Frame) lastParagraph() int32 {
	for p := f.firstP; ; {
		if next := f.paragraphs[p].next; next != 0 {
			p = next
			continue
		}
		return p
	}
}

func (f *Frame) firstParagraph() int32 {
	// TODO: move this check into the exported methods instead of this imported
	// one.
	if f.firstP == 0 {
		// 0 means that the first Paragraph is implicit (and not allocated
		// yet), so we make an explicit one, with a non-zero index.
		f.firstP = f.newParagraph()
	}
	return f.firstP
}

// FirstParagraph returns the first paragraph of this frame.
func (f *Frame) FirstParagraph() *Paragraph {
	return &f.paragraphs[f.firstParagraph()]
}

// Len returns the number of bytes in the Frame's text.
func (f *Frame) Len() int {
	return f.len
}

// NewCaret returns a new Caret at the start of this Frame.
func (f *Frame) NewCaret() *Caret {
	// Make the first Paragraph, Line and Box explicit, since Carets require an
	// explicit p, l and b.
	p := f.FirstParagraph()
	l := p.FirstLine(f)
	b := l.FirstBox(f)
	c := &Caret{
		f:           f,
		p:           f.firstP,
		l:           p.firstL,
		b:           l.firstB,
		k:           b.i,
		caretsIndex: len(f.carets),
	}
	f.carets = append(f.carets, c)
	return c
}

func (f *Frame) lineReader(b, k int32) lineReader {
	f.lineReaderData.b = b
	f.lineReaderData.k = k
	return lineReader{f}
}

// bAndK is a text position k within a Box b. The k is analogous to the Caret.k
// field. For a bAndK x, letting bb := Frame.boxes[x.b], an invariant is that
// bb.i <= x.k && x.k <= bb.j.
type bAndK struct {
	b int32
	k int32
}

// lineReader is an io.RuneReader for a Line of text, from its current position
// (a bAndK) up until the end of the Line containing that Box.
//
// A Frame can have only one active lineReader at any one time. To avoid
// excessive memory allocation and garbage collection, the lineReader's data is
// a field of the Frame struct and re-used.
type lineReader struct{ f *Frame }

func (z lineReader) bAndK() bAndK {
	return z.f.lineReaderData
}

func (z lineReader) ReadRune() (r rune, size int, err error) {
	d := &z.f.lineReaderData
	for d.b != 0 {
		bb := &z.f.boxes[d.b]
		if d.k < bb.j {
			r, size = utf8.DecodeRune(z.f.text[d.k:bb.j])
			if r >= utf8.RuneSelf && size == 1 {
				// We decoded invalid UTF-8, possibly because a valid UTF-8 rune
				// straddled this box and the next one.
				panic("TODO: invalid UTF-8")
			}
			d.k += int32(size)
			return r, size, nil
		}
		d.b = bb.next
		d.k = z.f.boxes[d.b].i
	}
	return 0, 0, io.EOF
}

// Paragraph holds Lines of text.
type Paragraph struct {
	firstL, next, prev int32
}

func (p *Paragraph) lastLine(f *Frame) int32 {
	for l := p.firstL; ; {
		if next := f.lines[l].next; next != 0 {
			l = next
			continue
		}
		return l
	}
}

func (p *Paragraph) firstLine(f *Frame) int32 {
	if p.firstL == 0 {
		// 0 means that the first Line is implicit (and not allocated yet), so
		// we make an explicit one, with a non-zero index.
		p.firstL = f.newLine()
	}
	return p.firstL
}

// FirstLine returns the first Line of this Paragraph.
//
// f is the Frame that contains the Paragraph.
func (p *Paragraph) FirstLine(f *Frame) *Line {
	return &f.lines[p.firstLine(f)]
}

// Next returns the next Paragraph after this one in the Frame.
//
// f is the Frame that contains the Paragraph.
func (p *Paragraph) Next(f *Frame) *Paragraph {
	if p.next == 0 {
		return nil
	}
	return &f.paragraphs[p.next]
}

// Line holds Boxes of text.
type Line struct {
	firstB, next, prev int32
}

func (l *Line) lastBox(f *Frame) int32 {
	for b := l.firstB; ; {
		if next := f.boxes[b].next; next != 0 {
			b = next
			continue
		}
		return b
	}
}

func (l *Line) firstBox(f *Frame) int32 {
	if l.firstB == 0 {
		// 0 means that the first Box is implicit (and not allocated yet), so
		// we make an explicit one, with a non-zero index.
		l.firstB = f.newBox()
	}
	return l.firstB
}

// FirstBox returns the first Box of this Line.
//
// f is the Frame that contains the Line.
func (l *Line) FirstBox(f *Frame) *Box {
	return &f.boxes[l.firstBox(f)]
}

// Next returns the next Line after this one in the Paragraph.
//
// f is the Frame that contains the Line.
func (l *Line) Next(f *Frame) *Line {
	if l.next == 0 {
		return nil
	}
	return &f.lines[l.next]
}

// Box holds a contiguous run of text.
type Box struct {
	next, prev int32
	// Frame.text[i:j] holds this Box's text.
	i, j int32
}

// Next returns the next Box after this one in the Line.
//
// f is the Frame that contains the Box.
func (b *Box) Next(f *Frame) *Box {
	if b.next == 0 {
		return nil
	}
	return &f.boxes[b.next]
}

// Text returns the Box's text.
//
// f is the Frame that contains the Box.
func (b *Box) Text(f *Frame) []byte {
	return f.text[b.i:b.j]
}

// TrimmedText returns the Box's text, trimmed right of any white space if it
// is the last Box in its Line.
//
// f is the Frame that contains the Box.
func (b *Box) TrimmedText(f *Frame) []byte {
	s := f.text[b.i:b.j]
	if b.next == 0 {
		for len(s) > 0 && s[len(s)-1] <= ' ' {
			s = s[:len(s)-1]
		}
	}
	return s
}
