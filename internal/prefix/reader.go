// Copyright 2015, Joe Tsai. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package prefix

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	"github.com/dsnet/compress"
	"github.com/dsnet/compress/internal"
	"github.com/dsnet/compress/internal/errors"
)

// Reader implements a prefix decoder. If the input io.Reader satisfies the
// compress.ByteReader or compress.BufferedReader interface, then it also
// guarantees that it will never read more bytes than is necessary.
//
// For high performance, provide an io.Reader that satisfies the
// compress.BufferedReader interface. If the input does not satisfy either
// compress.ByteReader or compress.BufferedReader, then it will be internally
// wrapped with a bufio.Reader.
type Reader struct {
	Offset int64 // Number of bytes read from the underlying io.Reader

	rd     io.Reader
	byteRd compress.ByteReader     // Set if rd is a ByteReader
	bufRd  compress.BufferedReader // Set if rd is a BufferedReader

	bufBits   uint64    // Buffer to hold some bits
	numBits   uint      // Number of valid bits in bufBits
	transform [256]byte // LUT to transform bit-ordering

	// These fields are only used if rd is a compress.BufferedReader.
	bufPeek     []byte // Buffer for the Peek data
	discardBits int    // Number of bits to discard from reader
	fedBits     uint   // Number of bits fed in last call to PullBits

	// These fields are used to reduce allocations.
	bb *buffer
	br *bytesReader
	sr *stringReader
	bu *bufio.Reader
}

// Init initializes the bit Reader to read from r. If bigEndian is true, then
// bits will be read starting from the most-significant bits of a byte
// (as done in bzip2), otherwise it will read starting from the
// least-significant bits of a byte (such as for deflate and brotli).
func (pr *Reader) Init(r io.Reader, bigEndian bool) {
	*pr = Reader{
		rd: r,

		bb: pr.bb,
		br: pr.br,
		sr: pr.sr,
		bu: pr.bu,
	}
	switch rr := r.(type) {
	case *bytes.Buffer:
		if pr.bb == nil {
			pr.bb = new(buffer)
		}
		*pr.bb = buffer{Buffer: rr}
		pr.bufRd = pr.bb
	case *bytes.Reader:
		if pr.br == nil {
			pr.br = new(bytesReader)
		}
		*pr.br = bytesReader{Reader: rr}
		pr.bufRd = pr.br
	case *strings.Reader:
		if pr.sr == nil {
			pr.sr = new(stringReader)
		}
		*pr.sr = stringReader{Reader: rr}
		pr.bufRd = pr.sr
	case compress.BufferedReader:
		pr.bufRd = rr
	case compress.ByteReader:
		pr.byteRd = rr
	default:
		if pr.bu == nil {
			pr.bu = bufio.NewReader(nil)
		}
		pr.bu.Reset(r)
		pr.rd, pr.bufRd = pr.bu, pr.bu
	}

	if bigEndian {
		copy(pr.transform[:], internal.ReverseLUT[:])
	} else {
		copy(pr.transform[:], internal.IdentityLUT[:])
	}
}

// BitsRead reports the total number of bits emitted from any Read method.
func (pr *Reader) BitsRead() int64 {
	offset := 8*pr.Offset - int64(pr.numBits)
	if pr.bufRd != nil {
		discardBits := pr.discardBits + int(pr.fedBits-pr.numBits)
		offset = 8*pr.Offset + int64(discardBits)
	}
	return offset
}

// IsBufferedReader reports whether the underlying io.Reader is also a
// compress.BufferedReader.
func (pr *Reader) IsBufferedReader() bool {
	return pr.bufRd != nil
}

// ReadPads reads 0-7 bits from the bit buffer to achieve byte-alignment.
func (pr *Reader) ReadPads() uint {
	nb := pr.numBits % 8
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val
}

// Read reads bytes into buf.
// The bit-ordering mode does not affect this method.
func (pr *Reader) Read(buf []byte) (cnt int, err error) {
	if pr.numBits > 0 {
		if pr.numBits%8 != 0 {
			return 0, errUnaligned
		}
		for cnt = 0; len(buf) > cnt && pr.numBits > 0; cnt++ {
			buf[cnt] = pr.transform[byte(pr.bufBits)]
			pr.bufBits >>= 8
			pr.numBits -= 8
		}
		return cnt, nil
	}
	if _, err := pr.Flush(); err != nil {
		return 0, err
	}
	cnt, err = pr.rd.Read(buf)
	pr.Offset += int64(cnt)
	return cnt, err
}

// ReadOffset reads an offset value using the provided RangeCodes indexed by
// the symbol read.
func (pr *Reader) ReadOffset(pd *Decoder, rcs RangeCodes) uint {
	rc := rcs[pr.ReadSymbol(pd)]
	return uint(rc.Base) + pr.ReadBits(uint(rc.Len))
}

// TryReadBits attempts to read nb bits using the contents of the bit buffer
// alone. It returns the value and whether it succeeded.
//
// This method is designed to be inlined for performance reasons.
func (pr *Reader) TryReadBits(nb uint) (uint, bool) {
	if pr.numBits < nb {
		return 0, false
	}
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val, true
}

// ReadBits reads nb bits in from the underlying reader.
func (pr *Reader) ReadBits(nb uint) uint {
	if err := pr.PullBits(nb); err != nil {
		errors.Panic(err)
	}
	val := uint(pr.bufBits & uint64(1<<nb-1))
	pr.bufBits >>= nb
	pr.numBits -= nb
	return val
}

// TryReadSymbol attempts to decode the next symbol using the contents of the
// bit buffer alone. It returns the decoded symbol and whether it succeeded.
//
// This method is designed to be inlined for performance reasons.
func (pr *Reader) TryReadSymbol(pd *Decoder) (uint, bool) {
	if pr.numBits < uint(pd.MinBits) || len(pd.chunks) == 0 {
		return 0, false
	}
	chunk := pd.chunks[uint32(pr.bufBits)&pd.chunkMask]
	nb := uint(chunk & countMask)
	if nb > pr.numBits || nb > uint(pd.chunkBits) {
		return 0, false
	}
	pr.bufBits >>= nb
	pr.numBits -= nb
	return uint(chunk >> countBits), true
}

// ReadSymbol reads the next symbol using the provided prefix Decoder.
func (pr *Reader) ReadSymbol(pd *Decoder) uint {
	if len(pd.chunks) == 0 {
		errors.Panic(errInvalid) // Decode with empty tree
	}

	nb := uint(pd.MinBits)
	for {
		if err := pr.PullBits(nb); err != nil {
			errors.Panic(err)
		}
		chunk := pd.chunks[uint32(pr.bufBits)&pd.chunkMask]
		nb = uint(chunk & countMask)
		if nb > uint(pd.chunkBits) {
			linkIdx := chunk >> countBits
			chunk = pd.links[linkIdx][uint32(pr.bufBits>>pd.chunkBits)&pd.linkMask]
			nb = uint(chunk & countMask)
		}
		if nb <= pr.numBits {
			pr.bufBits >>= nb
			pr.numBits -= nb
			return uint(chunk >> countBits)
		}
	}
}

// Flush updates the read offset of the underlying ByteReader.
// If reader is a compress.BufferedReader, then this calls Discard to update
// the read offset.
func (pr *Reader) Flush() (int64, error) {
	if pr.bufRd == nil {
		return pr.Offset, nil
	}

	// Update the number of total bits to discard.
	pr.discardBits += int(pr.fedBits - pr.numBits)
	pr.fedBits = pr.numBits

	// Discard some bytes to update read offset.
	var err error
	nd := (pr.discardBits + 7) / 8 // Round up to nearest byte
	nd, err = pr.bufRd.Discard(nd)
	pr.discardBits -= nd * 8 // -7..0
	pr.Offset += int64(nd)

	// These are invalid after Discard.
	pr.bufPeek = nil
	return pr.Offset, err
}

// PullBits ensures that at least nb bits exist in the bit buffer.
// If the underlying reader is a compress.BufferedReader, then this will fill
// the bit buffer with as many bits as possible, relying on Peek and Discard to
// properly advance the read offset. Otherwise, it will use ReadByte to fill the
// buffer with just the right number of bits.
func (pr *Reader) PullBits(nb uint) error {
	if pr.bufRd != nil {
		pr.discardBits += int(pr.fedBits - pr.numBits)
		for {
			if len(pr.bufPeek) == 0 {
				pr.fedBits = pr.numBits // Don't discard bits just added
				if _, err := pr.Flush(); err != nil {
					return err
				}

				var err error
				cntPeek := 8 // Minimum Peek amount to make progress
				if pr.bufRd.Buffered() > cntPeek {
					cntPeek = pr.bufRd.Buffered()
				}
				pr.bufPeek, err = pr.bufRd.Peek(cntPeek)
				pr.bufPeek = pr.bufPeek[int(pr.numBits/8):] // Skip buffered bits
				if len(pr.bufPeek) == 0 {
					if pr.numBits >= nb {
						break
					}
					if err == io.EOF {
						err = io.ErrUnexpectedEOF
					}
					return err
				}
			}
			cnt := int(64-pr.numBits) / 8
			if cnt > len(pr.bufPeek) {
				cnt = len(pr.bufPeek)
			}
			for _, c := range pr.bufPeek[:cnt] {
				pr.bufBits |= uint64(pr.transform[c]) << pr.numBits
				pr.numBits += 8
			}
			pr.bufPeek = pr.bufPeek[cnt:]
			if pr.numBits > 56 {
				break
			}
		}
		pr.fedBits = pr.numBits
	} else {
		for pr.numBits < nb {
			c, err := pr.byteRd.ReadByte()
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return err
			}
			pr.bufBits |= uint64(pr.transform[c]) << pr.numBits
			pr.numBits += 8
			pr.Offset++
		}
	}
	return nil
}
