package compare

import (
	"crypto/md5"
	"encoding/hex"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
