package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFUSECredentialsCloneAndZeroOwnTheirBuffers(t *testing.T) {
	source := FUSECredentials{AccessKey: []byte("access-a"), SecretKey: []byte("secret-a")}
	clone := source.Clone()

	clone.AccessKey[0] = 'X'
	clone.SecretKey[0] = 'Y'
	assert.Equal(t, []byte("access-a"), source.AccessKey)
	assert.Equal(t, []byte("secret-a"), source.SecretKey)

	clone.Zero()
	assert.Nil(t, clone.AccessKey)
	assert.Nil(t, clone.SecretKey)
	assert.Equal(t, []byte("access-a"), source.AccessKey)
	assert.Equal(t, []byte("secret-a"), source.SecretKey)
}
