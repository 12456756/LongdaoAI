package dto

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSEPKeysAreMaskedInPublicDTO(t *testing.T) {
	key := &service.APIKey{Key: "sk-sep-0123456789abcdef0123456789abcdef"}
	result := APIKeyFromService(key)
	require.Equal(t, "sk-sep-012...cdef", result.Key)
	require.Equal(t, "sk-sep-0123456789abcdef0123456789abcdef", key.Key)
	require.Equal(t, "ordinary-key", APIKeyFromService(&service.APIKey{Key: "ordinary-key"}).Key)
}
