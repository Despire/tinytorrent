package bencoding_test

import (
	"strings"
	"testing"

	"github.com/Despire/tinytorrent/bencoding"
	"github.com/stretchr/testify/assert"
)

func TestDecodeEncode(t *testing.T) {
	str := "d8:announce41:http://bttracker.debian.org:6969/announce7:comment35:\"Debian CD from cdimage.debian.org\"13:creation datei1391870037e9:httpseedsl85:http://cdimage.debian.org/cdimage/release/7.4.0/iso-cd/debian-7.4.0-amd64-netinst.iso85:http://cdimage.debian.org/cdimage/archive/7.4.0/iso-cd/debian-7.4.0-amd64-netinst.isoe4:infod6:lengthi232783872e4:name30:debian-7.4.0-amd64-netinst.iso12:piece lengthi262144e6:pieces0:ee"
	v, err := bencoding.Decode(strings.NewReader(str))
	assert.Nil(t, err)
	assert.Equal(t, str, v.Literal())
}
