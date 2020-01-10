package googledriveblobstore

import (
	"github.com/function61/gokit/assert"
	"github.com/function61/varasto/pkg/stotypes"
	"testing"
)

func TestToGoogleDriveName(t *testing.T) {
	ref, _ := stotypes.BlobRefFromHex("d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592")

	assert.EqualString(t, toGoogleDriveName(*ref), "16j7swfXgJRpypq8sAguT41WUeRtPNt2LQLQvzfJ5ZI")
}

func TestSerializeAndDeserializeConfig(t *testing.T) {
	serialized, err := (&Config{
		VarastoDirectoryId:    "vdi",
		GoogleCredentialsJson: "stfu",
	}).Serialize()
	assert.Assert(t, err == nil)

	assert.EqualString(t, serialized, `{"VarastoDirectoryId":"vdi","GoogleCredentialsJson":"stfu"}`)

	parsed, err := deserializeConfig(serialized)
	assert.Assert(t, err == nil)

	assert.EqualString(t, parsed.VarastoDirectoryId, "vdi")
	assert.EqualString(t, parsed.GoogleCredentialsJson, "stfu")
}
