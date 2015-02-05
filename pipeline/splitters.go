/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012-2015
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#   Mike Trinkala (trink@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/mozilla-services/heka/message"
	"hash"
	"regexp"
)

type NullSplitter struct {
}

func (n *NullSplitter) Init(config interface{}) error {
	return nil
}

func (n *NullSplitter) FindRecord(buf []byte) (bytesRead int, record []byte) {
	return len(buf), buf
}

type TokenSplitter struct {
	delimiter byte
}

type TokenSplitterConfig struct {
	Delimiter string
}

func (t *TokenSplitter) ConfigStruct() interface{} {
	return &TokenSplitterConfig{
		Delimiter: "\n",
	}
}

func (t *TokenSplitter) Init(config interface{}) error {
	conf := config.(*TokenSplitterConfig)
	if len(conf.Delimiter) != 1 {
		return errors.New("TokenSplitter delimiter must be a single character.")
	}
	t.delimiter = byte(conf.Delimiter[0])
	return nil
}

func (t *TokenSplitter) FindRecord(buf []byte) (bytesRead int, record []byte) {
	n := bytes.IndexByte(buf, t.delimiter)
	if n == -1 {
		return 0, nil
	}
	bytesRead = n + 1 // Include the delimiter in what's been read.
	return bytesRead, buf[:bytesRead]
}

type RegexSplitter struct {
	delimiter  *regexp.Regexp
	eol        bool
	captureLen int
}

type RegexSplitterConfig struct {
	Delimiter    string
	DelimiterEOL bool `toml:"delimiter_eol"`
}

func (r *RegexSplitter) ConfigStruct() interface{} {
	return &RegexSplitterConfig{
		Delimiter:    "\n",
		DelimiterEOL: true,
	}
}

func (r *RegexSplitter) Init(config interface{}) error {
	conf := config.(*RegexSplitterConfig)
	var err error
	if r.delimiter, err = regexp.Compile(conf.Delimiter); err != nil {
		return err
	}
	if r.delimiter.NumSubexp() > 1 {
		return fmt.Errorf("regex must not contain more than one capture group: %s",
			conf.Delimiter)
	}
	r.eol = conf.DelimiterEOL
	return nil
}

func (r *RegexSplitter) FindRecord(buf []byte) (bytesRead int, record []byte) {
	var loc []int
	loc = r.delimiter.FindSubmatchIndex(buf[r.captureLen:])
	if loc == nil {
		return 0, nil
	}
	if len(loc) == 4 {
		if r.eol { // append the capture to the end of the previous record
			record = buf[:loc[3]]
			bytesRead = loc[1]
		} else { // append the capture to the beginning of the next record
			record = buf[:loc[0]+r.captureLen]
			bytesRead = loc[3] + r.captureLen
			r.captureLen = loc[3] - loc[2]
			bytesRead -= r.captureLen
		}
	} else { // no capture discard the delimiter
		record = buf[:loc[0]]
		bytesRead = loc[1]
	}
	return bytesRead, record
}

// Heka Message signer object.
type Signer struct {
	HmacKey string `toml:"hmac_key"`
}

// Returns true if the provided message is unsigned or has a valid signature
// from one of the provided signers.
func authenticateMessage(signers map[string]Signer, header *message.Header,
	msg []byte) bool {

	digest := header.GetHmac()
	if digest != nil {
		var key string
		signer := fmt.Sprintf("%s_%d", header.GetHmacSigner(),
			header.GetHmacKeyVersion())
		if s, ok := signers[signer]; ok {
			key = s.HmacKey
		} else {
			return false
		}

		var hm hash.Hash
		switch header.GetHmacHashFunction() {
		case message.Header_MD5:
			hm = hmac.New(md5.New, []byte(key))
		case message.Header_SHA1:
			hm = hmac.New(sha1.New, []byte(key))
		}
		hm.Write(msg)
		expectedDigest := hm.Sum(nil)
		if subtle.ConstantTimeCompare(digest, expectedDigest) != 1 {
			return false
		}
	}
	return true
}

type HekaFramingSplitter struct {
	*HekaFramingSplitterConfig
	header *message.Header
	sr     SplitterRunner
}

type HekaFramingSplitterConfig struct {
	// Set of message signer objects, keyed by signer id string.
	Signers     map[string]Signer `toml:"signer"`
	UseMsgBytes bool              `toml:"use_message_bytes"`
	SkipAuth    bool              `toml:"skip_authentication"`
}

func (h *HekaFramingSplitter) SetSplitterRunner(sr SplitterRunner) {
	h.sr = sr
}

func (h *HekaFramingSplitter) ConfigStruct() interface{} {
	return &HekaFramingSplitterConfig{
		UseMsgBytes: true,
	}
}

func (h *HekaFramingSplitter) Init(config interface{}) error {
	h.HekaFramingSplitterConfig = config.(*HekaFramingSplitterConfig)
	h.header = &message.Header{}
	return nil
}

func (h *HekaFramingSplitter) FindRecord(buf []byte) (bytesRead int, record []byte) {
	bytesRead = bytes.IndexByte(buf, message.RECORD_SEPARATOR)
	if bytesRead == -1 {
		bytesRead = len(buf)
		return // read more data to find the start of the next message
	}

	if len(buf) < bytesRead+message.HEADER_DELIMITER_SIZE {
		return // read more data to get the header length byte
	}
	headerLength := int(buf[bytesRead+1])
	headerEnd := bytesRead + headerLength + message.HEADER_FRAMING_SIZE
	if len(buf) < headerEnd {
		return // read more data to get the remainder of the header
	}
	decoded, err := message.DecodeHeader(
		buf[bytesRead+message.HEADER_DELIMITER_SIZE:headerEnd], h.header)
	if err != nil {
		h.sr.LogError(err)
	}
	if h.header.MessageLength != nil || decoded {
		messageEnd := headerEnd + int(h.header.GetMessageLength())
		if len(buf) < messageEnd {
			return // read more data to get the remainder of the message
		}
		record = buf[bytesRead:messageEnd]
		bytesRead = messageEnd
		h.header.Reset()
	} else {
		var n int
		bytesRead++                               // advance over the current record separator
		n, record = h.FindRecord(buf[bytesRead:]) // header was invalid, look again
		bytesRead += n
	}
	return bytesRead, record
}

func (h *HekaFramingSplitter) UnframeRecord(framed []byte, pack *PipelinePack) []byte {
	headerLen := int(framed[1]) + message.HEADER_FRAMING_SIZE
	unframed := framed[headerLen:]
	if !h.SkipAuth && headerLen > message.UUID_SIZE {
		header := &message.Header{}
		decoded, err := message.DecodeHeader(framed[2:headerLen], header)
		if err != nil {
			h.sr.LogError(err)
		}
		if decoded && authenticateMessage(h.Signers, header, unframed) {
			pack.Signer = header.GetHmacSigner()
		} else {
			return nil
		}
	}
	return unframed
}

func init() {
	RegisterPlugin("NullSplitter", func() interface{} {
		return &NullSplitter{}
	})
	RegisterPlugin("TokenSplitter", func() interface{} {
		return &TokenSplitter{}
	})
	RegisterPlugin("RegexSplitter", func() interface{} {
		return &RegexSplitter{}
	})
	RegisterPlugin("HekaFramingSplitter", func() interface{} {
		return &HekaFramingSplitter{}
	})
}
