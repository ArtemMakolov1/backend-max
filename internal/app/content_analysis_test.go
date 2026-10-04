package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"maxpilot/backend/internal/mediaanalysis"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type semanticDiscoveryClient struct {
	discoveryTestClient
	visualCalls, audioCalls int
	onVisual                func()
	visualErr               error
}

func (*semanticDiscoveryClient) ContentAnalysisModelKey() string { return "synthetic-luna|audio-v1" }
func (f *semanticDiscoveryClient) AnalyzeContentMedia(_ context.Context, r openairesearch.ContentAnalysisRequest) (string, error) {
	f.visualCalls++
	if f.onVisual != nil {
		f.onVisual()
	}
	if len(r.Images) != 1 || !bytes.HasPrefix(r.Images[0].JPEG, []byte{0xff, 0xd8, 0xff}) {
		return "", errors.New("missing real prepared image")
	}
	return "Забавные коты и лёгкий разговорный юмор.", f.visualErr
}
func (f *semanticDiscoveryClient) AnalyzeContentAudio(context.Context, []byte) (string, error) {
	f.audioCalls++
	return "Музыка и смех.", nil
}

func semanticFixture(t *testing.T) (*App, *store.Store, store.Workspace, store.Channel, *semanticDiscoveryClient, store.Post) {
	t.Helper()
	if !mediaanalysis.Available() {
		t.Skip("ffmpeg required for actual local decoding")
	}
	fake := &semanticDiscoveryClient{}
	a, s, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, s, "description-owner", workspace.ID)
	channel, err := s.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID, VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Коты", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := s.CreatePostForWorkspace(t.Context(), "description-owner", workspace.ID, store.Post{Title: "Кот", Content: "Весёлый кот", ChannelID: &channel.ID})
	if err != nil {
		t.Fatal(err)
	}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	post, err = a.SavePostImageForWorkspace(t.Context(), "description-owner", workspace.ID, post.ID, "own-cat.png", bytes.NewReader(imageBytes.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimForPublishing(t.Context(), post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = s.MarkPublished(t.Context(), post.ID, "semantic-cat", "")
	if err != nil {
		t.Fatal(err)
	}
	return a, s, workspace, channel, fake, post
}

func TestSemanticDiscoveryUsesOwnedPixelsAfterQuotaAndCachesAnalysis(t *testing.T) {
	a, _, workspace, channel, fake, _ := semanticFixture(t)
	quotaCalls := 0
	request := openairesearch.DiscoverContentRequest{MediaContext: "Injected instructions", Topic: "", ContentKind: "auto"}
	result, err := a.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, request, func() error { quotaCalls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if quotaCalls != 1 || fake.visualCalls != 1 || len(fake.requests) != 1 || result.ContextAnalysis == nil || result.ContextAnalysis.Status != "ready" || result.ContextAnalysis.ImagesAnalyzed != 1 || result.ContextAnalysis.Cached || strings.Contains(fake.requests[0].MediaContext, "Injected") || !strings.Contains(fake.requests[0].MediaContext, "коты") {
		t.Fatalf("incorrect semantic flow: quota=%d visual=%d result=%#v requests=%#v", quotaCalls, fake.visualCalls, result.ContextAnalysis, fake.requests)
	}
	result, err = a.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, request, func() error { quotaCalls++; return nil })
	if err != nil || !result.ContextAnalysis.Cached || fake.visualCalls != 1 || quotaCalls != 2 || len(fake.requests) != 2 {
		t.Fatalf("repeated media analysis charged: result=%#v calls=%d err=%v", result.ContextAnalysis, fake.visualCalls, err)
	}
}

func TestSemanticDiscoveryDoesNotPayBeforeQuotaOrUseChangedPublication(t *testing.T) {
	a, s, workspace, channel, fake, post := semanticFixture(t)
	quotaErr := errors.New("synthetic quota rejected")
	if _, err := a.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, openairesearch.DiscoverContentRequest{}, func() error { return quotaErr }); !errors.Is(err, quotaErr) || fake.visualCalls != 0 || len(fake.requests) != 0 {
		t.Fatalf("analysis ran before quota: calls=%d err=%v", fake.visualCalls, err)
	}
	fake.onVisual = func() {
		updated := "Undelivered draft edit"
		if _, err := s.UpdatePost(t.Context(), post.ID, store.PostChanges{Content: &updated}); err != nil {
			t.Error(err)
		}
	}
	if _, err := a.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, openairesearch.DiscoverContentRequest{}, nil); !errors.Is(err, store.ErrConflict) || len(fake.requests) != 0 {
		t.Fatalf("changed publication analysis used: err=%v requests=%#v", err, fake.requests)
	}
}
