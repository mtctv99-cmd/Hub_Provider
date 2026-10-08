package meta

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"sever_provider/internal/provider"
)

// Adapter implements provider.Adapter for Meta / muse.ai.
type Adapter struct {
	Client       *Client
	activeLeases atomic.Int32
}

func New(client *Client) *Adapter {
	if client == nil {
		client = NewClient()
	}
	return &Adapter{
		Client: client,
	}
}

func (a *Adapter) Name() string {
	return "meta_web"
}

func (a *Adapter) Capabilities() []provider.Capability {
	return []provider.Capability{
		provider.CapabilityChat,
		provider.CapabilityStreaming,
		provider.CapabilityVision,
		provider.CapabilityImage,
		provider.CapabilitySpeech,
		provider.CapabilityVideo,
	}
}

func (a *Adapter) ActiveLeases() int32 {
	return a.activeLeases.Load()
}

func (a *Adapter) AcquireLease() {
	a.activeLeases.Add(1)
}

func (a *Adapter) ReleaseLease() {
	a.activeLeases.Add(-1)
}

type CredentialSecret struct {
	Cookies string `json:"cookies"`
	Token   string `json:"token"`
}

func parseCredentialSecret(secretBytes []byte) (cookies, token string, err error) {
	if len(secretBytes) == 0 {
		return "", "", errors.New("empty credential secret")
	}
	var cred CredentialSecret
	if err := json.Unmarshal(secretBytes, &cred); err == nil {
		return strings.TrimSpace(cred.Cookies), strings.TrimSpace(cred.Token), nil
	}
	raw := strings.TrimSpace(string(secretBytes))
	if strings.Contains(raw, "=") {
		return raw, "", nil
	}
	return "", raw, nil
}

func (a *Adapter) Chat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (provider.ChatResponse, error) {
	cookies, token, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return provider.ChatResponse{}, err
	}

	a.AcquireLease()
	defer a.ReleaseLease()

	reply, err := a.Client.SendMessage(ctx, req, cookies, token)
	if err != nil {
		return provider.ChatResponse{}, err
	}

	if strings.TrimSpace(reply.Text) == "" {
		return provider.ChatResponse{}, provider.NewEmptyResponseError(provider.EmptyReasonUpstreamEmpty, "meta chat returned empty text")
	}

	finishReason := "stop"
	return provider.ChatResponse{
		ID:           "meta-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Model:        req.Model,
		Content:      reply.Text,
		FinishReason: finishReason,
		Usage:        provider.ComputeUsage(req.Messages, req.Tools, reply.Text, "", nil, nil),
		Upstream: provider.UpstreamState{
			ChatSessionID: reply.SessionID,
			TurnState:     reply.TurnID,
		},
	}, nil
}

func (a *Adapter) StreamChat(ctx context.Context, req provider.ChatRequest, credentialSecret []byte) (<-chan provider.StreamChunk, error) {
	cookies, token, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return nil, err
	}

	out := make(chan provider.StreamChunk, 16)
	go func() {
		defer close(out)
		a.AcquireLease()
		defer a.ReleaseLease()

		ch, err := a.Client.StreamMessage(ctx, req, cookies, token)
		if err != nil {
			out <- provider.StreamChunk{Err: err}
			return
		}

		for chunk := range ch {
			select {
			case <-ctx.Done():
				out <- provider.StreamChunk{Err: ctx.Err()}
				return
			case out <- chunk:
			}
		}
	}()

	return out, nil
}

func (a *Adapter) Speech(ctx context.Context, req provider.SpeechRequest, credentialSecret []byte) ([]byte, string, error) {
	cookies, token, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return nil, "", err
	}

	a.AcquireLease()
	defer a.ReleaseLease()

	return a.Client.SynthesizeSpeech(ctx, req, cookies, token)
}

func (a *Adapter) Image(ctx context.Context, req provider.ImageRequest, credentialSecret []byte) (provider.ImageResponse, error) {
	cookies, token, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return provider.ImageResponse{}, err
	}

	a.AcquireLease()
	defer a.ReleaseLease()

	imageURL, err := a.Client.GenerateImage(ctx, req, cookies, token)
	if err != nil {
		return provider.ImageResponse{}, err
	}

	return provider.ImageResponse{
		URL: imageURL,
	}, nil
}

func (a *Adapter) Video(ctx context.Context, req provider.VideoRequest, credentialSecret []byte) (provider.VideoResponse, error) {
	cookies, token, err := parseCredentialSecret(credentialSecret)
	if err != nil {
		return provider.VideoResponse{}, err
	}

	a.AcquireLease()
	defer a.ReleaseLease()

	videoURL, err := a.Client.GenerateVideo(ctx, req, cookies, token)
	if err != nil {
		return provider.VideoResponse{}, err
	}

	return provider.VideoResponse{
		URL:  videoURL,
		MIME: "video/mp4",
	}, nil
}
