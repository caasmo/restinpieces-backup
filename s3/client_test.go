package s3

import (
	"testing"

	"github.com/caasmo/restinpieces/config"
)

func TestNewClient_EmptyEndpoint(t *testing.T) {
	client, err := NewClient(config.S3{})
	if err == nil {
		t.Fatal("expected error when s3.endpoint is empty")
	}
	if client != nil {
		t.Fatalf("client = %v, want nil", client)
	}
}

func TestNewClient_MapsConfig(t *testing.T) {
	s3Config := config.S3{
		Endpoint:             "https://s3.example.com",
		Region:               "auto",
		AccessKey:            "ak",
		SecretKey:            "sk",
		UsePathStyle:         true,
		RequireContentLength: true,
	}

	client, err := NewClient(s3Config)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.Endpoint != s3Config.Endpoint {
		t.Errorf("Endpoint: got %q, want %q", client.Endpoint, s3Config.Endpoint)
	}
	if client.Region != s3Config.Region {
		t.Errorf("Region: got %q, want %q", client.Region, s3Config.Region)
	}
	if client.AccessKey != s3Config.AccessKey {
		t.Errorf("AccessKey: got %q, want %q", client.AccessKey, s3Config.AccessKey)
	}
	if client.SecretKey != s3Config.SecretKey {
		t.Errorf("SecretKey: got %q, want %q", client.SecretKey, s3Config.SecretKey)
	}
	if client.UsePathStyle != s3Config.UsePathStyle {
		t.Errorf("UsePathStyle: got %v, want %v", client.UsePathStyle, s3Config.UsePathStyle)
	}
	if client.RequireContentLength != s3Config.RequireContentLength {
		t.Errorf("RequireContentLength: got %v, want %v", client.RequireContentLength, s3Config.RequireContentLength)
	}
}
