// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shadowsocks

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
)

// payloadSizeMask is the maximum size of payload in bytes.
const payloadSizeMask = 0x3FFF // 16*1024 - 1

// SaltGenerator generates unique salts to use in Shadowsocks connections.
type SaltGenerator interface {
	// Returns a new salt
	GetSalt() ([]byte, error)
}

// randomSaltGenerator generates a new random salt.
type randomSaltGenerator struct {
	saltSize int
}

// ServerSaltGenerator generates unique salts that are secretly marked.
type ServerSaltGenerator struct {
	saltSize  int
	encrypter cipher.AEAD
}

// GetSalt outputs a random salt.
func (sg randomSaltGenerator) GetSalt() ([]byte, error) {
	salt := make([]byte, sg.saltSize)
	_, err := io.ReadFull(rand.Reader, salt)
	return salt, err
}

// Number of bytes of salt to use as a marker.
const markLen = 4

// Constant to identify this marking scheme.
var serverIndication = []byte("outline-salt-mark")

func NewServerSaltGenerator(cipher shadowaead.Cipher) (ServerSaltGenerator, error) {
	saltSize := cipher.SaltSize()
	zerosalt := make([]byte, saltSize)
	encrypter, err := cipher.Encrypter(zerosalt)
	if err != nil {
		return ServerSaltGenerator{}, err
	}
	return ServerSaltGenerator{saltSize, encrypter}, nil
}

func (sg ServerSaltGenerator) splitSalt(salt []byte) (prefix, mark []byte) {
	prefixLen := sg.saltSize - markLen
	prefix = salt[:prefixLen]
	mark = salt[prefixLen:]
	return
}

// getTag takes in a salt prefix and writes out the tag.
// len(prefix) must be saltSize - markLen
func (sg ServerSaltGenerator) getTag(prefix []byte) []byte {
	nonce := make([]byte, sg.encrypter.NonceSize())
	n := copy(nonce, prefix)
	plaintext := prefix[n:]
	encrypted := sg.encrypter.Seal(nil, nonce, plaintext, serverIndication)
	return encrypted[len(plaintext):]
}

// GetSalt returns an apparently random salt that can be identified
// as server-originated by anyone who knows the Shadowsocks key.
func (sg ServerSaltGenerator) GetSalt() ([]byte, error) {
	salt := make([]byte, sg.saltSize)
	prefix, mark := sg.splitSalt(salt)
	_, err := io.ReadFull(rand.Reader, prefix)
	if err != nil {
		return nil, err
	}
	tag := sg.getTag(prefix)
	copy(mark, tag)
	return salt, nil
}

// IsMarked returns true if the salt is marked as server-originated.
func (sg ServerSaltGenerator) IsMarked(salt []byte) bool {
	prefix, mark := sg.splitSalt(salt)
	tag := sg.getTag(prefix)
	return bytes.Equal(tag[:markLen], mark)
}

// Writer is an io.Writer that also implements io.ReaderFrom to
// allow for piping the data without extra allocations and copies.
// The LazyWrite and Flush methods allow a header to be
// added but delayed until the first write, for concatenation.
// All methods except Flush must be called from a single thread.
type Writer struct {
	// This type is single-threaded except when needFlush is true.
	// mu protects needFlush, and also protects everything
	// else while needFlush could be true.
	mu sync.Mutex
	// Indicates that a concurrent flush is currently allowed.
	needFlush     bool
	writer        io.Writer
	ssCipher      shadowaead.Cipher
	saltGenerator SaltGenerator
	// Wrapper for input that arrives as a slice.
	byteWrapper bytes.Reader
	// Number of plaintext bytes that are currently buffered.
	pending int
	// These are populated by init():
	buf  []byte
	aead cipher.AEAD
	// Index of the next encrypted chunk to write.
	counter []byte
}

// NewShadowsocksWriter creates a Writer that encrypts the given Writer using
// the shadowsocks protocol with the given shadowsocks cipher.
func NewShadowsocksWriter(writer io.Writer, ssCipher shadowaead.Cipher) *Writer {
	return &Writer{writer: writer, ssCipher: ssCipher, saltGenerator: randomSaltGenerator{ssCipher.SaltSize()}}
}

// SetSaltGenerator sets the salt generator to be used. Must be called before the first write.
func (sw *Writer) SetSaltGenerator(saltGenerator SaltGenerator) {
	sw.saltGenerator = saltGenerator
}

// init generates a random salt, sets up the AEAD object and writes
// the salt to the inner Writer.
func (sw *Writer) init() (err error) {
	if sw.aead == nil {
		salt, err := sw.saltGenerator.GetSalt()
		if err != nil {
			return fmt.Errorf("failed to generate salt: %v", err)
		}
		sw.aead, err = sw.ssCipher.Encrypter(salt)
		if err != nil {
			return fmt.Errorf("failed to create AEAD: %v", err)
		}
		sw.counter = make([]byte, sw.aead.NonceSize())
		// The maximum length message is the salt (first message only), length, length tag,
		// payload, and payload tag.
		sizeBufSize := 2 + sw.aead.Overhead()
		maxPayloadBufSize := payloadSizeMask + sw.aead.Overhead()
		sw.buf = make([]byte, len(salt)+sizeBufSize+maxPayloadBufSize)
		// Store the salt at the start of sw.buf.
		copy(sw.buf, salt)
	}
	return nil
}

// encryptBlock encrypts `plaintext` in-place.  The slice must have enough capacity
// for the tag. Returns the total ciphertext length.
func (sw *Writer) encryptBlock(plaintext []byte) int {
	out := sw.aead.Seal(plaintext[:0], sw.counter, plaintext, nil)
	increment(sw.counter)
	return len(out)
}

func (sw *Writer) Write(p []byte) (int, error) {
	sw.byteWrapper.Reset(p)
	n, err := sw.ReadFrom(&sw.byteWrapper)
	return int(n), err
}

// LazyWrite queues p to be written, but doesn't send it until Flush() is
// called, a non-lazy write is made, or the buffer is filled.
func (sw *Writer) LazyWrite(p []byte) (int, error) {
	if err := sw.init(); err != nil {
		return 0, err
	}

	// Locking is needed due to potential concurrency with the Flush()
	// for a previous call to LazyWrite().
	sw.mu.Lock()
	defer sw.mu.Unlock()

	queued := 0
	for {
		n := sw.enqueue(p)
		queued += n
		p = p[n:]
		if len(p) == 0 {
			sw.needFlush = true
			return queued, nil
		}
		// p didn't fit in the buffer.  Flush the buffer and try
		// again.
		if err := sw.flush(); err != nil {
			return queued, err
		}
	}
}

// Flush sends the pending data, if any.  This method is thread-safe.
func (sw *Writer) Flush() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !sw.needFlush {
		return nil
	}
	return sw.flush()
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// Returns the slices of sw.buf in which to place plaintext for encryption.
func (sw *Writer) buffers() (sizeBuf, payloadBuf []byte) {
	// sw.buf starts with the salt.
	saltSize := sw.ssCipher.SaltSize()

	// Each Shadowsocks-TCP message consists of a fixed-length size block,
	// followed by a variable-length payload block.
	sizeBuf = sw.buf[saltSize : saltSize+2]
	payloadStart := saltSize + 2 + sw.aead.Overhead()
	payloadBuf = sw.buf[payloadStart : payloadStart+payloadSizeMask]
	return
}

// ReadFrom implements the io.ReaderFrom interface.
func (sw *Writer) ReadFrom(r io.Reader) (int64, error) {
	if err := sw.init(); err != nil {
		return 0, err
	}
	var written int64
	var err error
	_, payloadBuf := sw.buffers()

	// Special case: one thread-safe read, if necessary
	sw.mu.Lock()
	if sw.needFlush {
		pending := sw.pending

		sw.mu.Unlock()
		saltsize := sw.ssCipher.SaltSize()
		overhead := sw.aead.Overhead()
		// The first pending+overhead bytes of payloadBuf are potentially
		// in use, and may be modified on the flush thread.  Data after
		// that is safe to use on this thread.
		readBuf := sw.buf[saltsize+2+overhead+pending+overhead:]
		var plaintextSize int
		plaintextSize, err = r.Read(readBuf)
		written = int64(plaintextSize)
		sw.mu.Lock()

		sw.enqueue(readBuf[:plaintextSize])
		if flushErr := sw.flush(); flushErr != nil {
			err = flushErr
		}
		sw.needFlush = false
	}
	sw.mu.Unlock()

	// Main transfer loop
	for err == nil {
		sw.pending, err = r.Read(payloadBuf)
		written += int64(sw.pending)
		if flushErr := sw.flush(); flushErr != nil {
			err = flushErr
		}
	}

	if err == io.EOF { // ignore EOF as per io.ReaderFrom contract
		return written, nil
	}
	return written, fmt.Errorf("Failed to read payload: %v", err)
}

// Adds as much of `plaintext` into the buffer as will fit, and increases
// sw.pending accordingly.  Returns the number of bytes consumed.
func (sw *Writer) enqueue(plaintext []byte) int {
	_, payloadBuf := sw.buffers()
	n := copy(payloadBuf[sw.pending:], plaintext)
	sw.pending += n
	return n
}

// Encrypts all pending data and writes it to the output.
func (sw *Writer) flush() error {
	if sw.pending == 0 {
		return nil
	}
	// sw.buf starts with the salt.
	saltSize := sw.ssCipher.SaltSize()
	// Normally we ignore the salt at the beginning of sw.buf.
	start := saltSize
	if isZero(sw.counter) {
		// For the first message, include the salt.  Compared to writing the salt
		// separately, this saves one packet during TCP slow-start and potentially
		// avoids having a distinctive size for the first packet.
		start = 0
	}

	sizeBuf, payloadBuf := sw.buffers()
	binary.BigEndian.PutUint16(sizeBuf, uint16(sw.pending))
	sizeBlockSize := sw.encryptBlock(sizeBuf)
	payloadSize := sw.encryptBlock(payloadBuf[:sw.pending])
	_, err := sw.writer.Write(sw.buf[start : saltSize+sizeBlockSize+payloadSize])
	sw.pending = 0
	return err
}

// ChunkReader is similar to io.Reader, except that it controls its own
// buffer granularity.
type ChunkReader interface {
	// ReadChunk reads the next chunk and returns its payload.  The caller must
	// complete its use of the returned buffer before the next call.
	// The buffer is nil iff there is an error.  io.EOF indicates a close.
	ReadChunk() ([]byte, error)
}

type chunkReader struct {
	reader   io.Reader
	ssCipher shadowaead.Cipher
	// These are lazily initialized:
	aead cipher.AEAD
	// Index of the next encrypted chunk to read.
	counter []byte
	buf     []byte
}

// Reader is an io.Reader that also implements io.WriterTo to
// allow for piping the data without extra allocations and copies.
type Reader interface {
	io.Reader
	io.WriterTo
}

// NewShadowsocksReader creates a Reader that decrypts the given Reader using
// the shadowsocks protocol with the given shadowsocks cipher.
func NewShadowsocksReader(reader io.Reader, ssCipher shadowaead.Cipher) Reader {
	return &readConverter{
		cr: &chunkReader{reader: reader, ssCipher: ssCipher},
	}
}

// init reads the salt from the inner Reader and sets up the AEAD object
func (cr *chunkReader) init() (err error) {
	if cr.aead == nil {
		// For chacha20-poly1305, SaltSize is 32, NonceSize is 12 and Overhead is 16.
		salt := make([]byte, cr.ssCipher.SaltSize())
		if _, err := io.ReadFull(cr.reader, salt); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				err = fmt.Errorf("failed to read salt: %v", err)
			}
			return err
		}
		cr.aead, err = cr.ssCipher.Decrypter(salt)
		if err != nil {
			return fmt.Errorf("failed to create AEAD: %v", err)
		}
		cr.counter = make([]byte, cr.aead.NonceSize())
		cr.buf = make([]byte, payloadSizeMask+cr.aead.Overhead())
	}
	return nil
}

// readMessage reads, decrypts, and verifies a single AEAD ciphertext.
// The ciphertext and tag (i.e. "overhead") must exactly fill `buf`,
// and the decrypted message will be placed in buf[:len(buf)-overhead].
// Returns an error only if the block could not be read.
func (cr *chunkReader) readMessage(buf []byte) error {
	_, err := io.ReadFull(cr.reader, buf)
	if err != nil {
		return err
	}
	_, err = cr.aead.Open(buf[:0], cr.counter, buf, nil)
	increment(cr.counter)
	if err != nil {
		return fmt.Errorf("failed to decrypt: %v", err)
	}
	return nil
}

func (cr *chunkReader) ReadChunk() ([]byte, error) {
	if err := cr.init(); err != nil {
		return nil, err
	}
	// In Shadowsocks-AEAD, each chunk consists of two
	// encrypted messages.  The first message contains the payload length,
	// and the second message is the payload.
	sizeBuf := cr.buf[:2+cr.aead.Overhead()]
	if err := cr.readMessage(sizeBuf); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			err = fmt.Errorf("failed to read payload size: %v", err)
		}
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(sizeBuf) & payloadSizeMask)
	sizeWithTag := size + cr.aead.Overhead()
	if cap(cr.buf) < sizeWithTag {
		// This code is unreachable.
		return nil, io.ErrShortBuffer
	}
	payloadBuf := cr.buf[:sizeWithTag]
	if err := cr.readMessage(payloadBuf); err != nil {
		if err == io.EOF { // EOF is not expected mid-chunk.
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payloadBuf[:size], nil
}

// readConverter adapts from ChunkReader, with source-controlled
// chunk sizes, to Go-style IO.
type readConverter struct {
	cr       ChunkReader
	leftover []byte
}

func (c *readConverter) Read(b []byte) (int, error) {
	if err := c.ensureLeftover(); err != nil {
		return 0, err
	}
	n := copy(b, c.leftover)
	c.leftover = c.leftover[n:]
	return n, nil
}

func (c *readConverter) WriteTo(w io.Writer) (written int64, err error) {
	for {
		if err = c.ensureLeftover(); err != nil {
			if err == io.EOF {
				err = nil
			}
			return written, err
		}
		n, err := w.Write(c.leftover)
		written += int64(n)
		c.leftover = c.leftover[n:]
		if err != nil {
			return written, err
		}
	}
}

// Ensures that c.leftover is nonempty.  If leftover is empty, this method
// waits for incoming data and decrypts it.
// Returns an error only if c.leftover could not be populated.
func (c *readConverter) ensureLeftover() error {
	if len(c.leftover) > 0 {
		return nil
	}
	payload, err := c.cr.ReadChunk()
	if err != nil {
		return err
	}
	c.leftover = payload
	return nil
}

// increment little-endian encoded unsigned integer b. Wrap around on overflow.
func increment(b []byte) {
	for i := range b {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}
