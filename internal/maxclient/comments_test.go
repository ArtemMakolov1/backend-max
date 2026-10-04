package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const sampleComment = `{"sender":{"user_id":9007199254740993,"first_name":"Анна","last_name":"Иванова","username":"anna","is_bot":false},"recipient":{"chat_id":-9007199254740993,"chat_type":"channel","post_id":"post-1"},"timestamp":1791129600123,"body":{"mid":"comment-2","seq":2,"text":"Спасибо"},"link":{"type":"reply","message":{"mid":"comment-1","seq":1,"text":"Родитель"}}}`

func TestGetCommentsPreservesLosslessIDsAndProviderOrder(t *testing.T) {
	t.Parallel()
	before, after := int64(1791200000000), int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/messages/post-1/comments" ||
			r.URL.Query().Get("count") != "2" || r.URL.Query().Get("before") != "1791200000000" ||
			r.URL.Query().Get("after") != "0" || r.Header.Get("Authorization") != "raw-token" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		_, _ = io.WriteString(w, `{"messages":[`+sampleComment+`,{"sender":null,"recipient":{"chat_id":"-9007199254740993","chat_type":"channel","post_id":null},"timestamp":1791129600000,"body":{"mid":"comment-1","seq":1,"text":"От канала"}}]}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL+"/api", "raw-token", server.Client())
	page, err := client.GetComments(context.Background(), "post-1", CommentsQuery{Count: 2, Before: &before, After: &after})
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("page=%+v error=%v", page, err)
	}
	newest := page.Messages[0]
	if newest.MessageID != "comment-2" || newest.ChatID != "-9007199254740993" || newest.PostID != "post-1" ||
		newest.TimestampMillis != 1791129600123 || newest.SenderUserID != "9007199254740993" ||
		newest.SenderName != "Анна Иванова" || newest.SenderUsername != "anna" || newest.ReplyTo != "comment-1" {
		t.Fatalf("newest comment=%+v", newest)
	}
	if page.Messages[1].MessageID != "comment-1" || page.Messages[1].PostID != "" || page.Messages[1].SenderUserID != "" {
		t.Fatalf("channel-authored comment=%+v", page.Messages[1])
	}
	encoded, err := json.Marshal(newest)
	if err != nil || strings.Contains(string(encoded), "seq") {
		t.Fatalf("provider raw data serialized: %s error=%v", encoded, err)
	}
}

func TestGetCommentsExplicitIDsUseCSVAndIgnorePagination(t *testing.T) {
	t.Parallel()
	boundary := int64(10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("comment_ids") != "comment-2,comment-1" || len(query) != 1 {
			t.Errorf("query=%v, want only comma-separated IDs", query)
		}
		_, _ = io.WriteString(w, `{"messages":[`+sampleComment+`]}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	page, err := client.GetComments(context.Background(), "post-1", CommentsQuery{CommentIDs: []string{"comment-2", "comment-1"}, Count: 1, Before: &boundary, After: &boundary})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("page=%+v error=%v", page, err)
	}
}

func TestGetCommentsDefaultBoundAndEmptyPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") != "50" || len(r.URL.Query()) != 1 {
			t.Errorf("default query=%v", r.URL.Query())
		}
		_, _ = io.WriteString(w, `{"messages":[]}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	page, err := client.GetComments(context.Background(), "post-1", CommentsQuery{})
	if err != nil || page.Messages == nil || len(page.Messages) != 0 {
		t.Fatalf("empty page=%+v, error=%v", page, err)
	}
}

func TestCommentsRejectInvalidInputsWithoutRequests(t *testing.T) {
	t.Parallel()
	transport := &countingTransport{}
	client := mustClient(t, "https://platform-api2.max.ru", "token", &http.Client{Transport: transport})
	negative := int64(-1)
	queries := []CommentsQuery{{Count: -1}, {Count: 101}, {Before: &negative}, {After: &negative},
		{CommentIDs: []string{"../other"}}, {CommentIDs: []string{"same", "same"}}, {CommentIDs: make([]string, 101)}}
	for _, query := range queries {
		if _, err := client.GetComments(context.Background(), "post-1", query); err == nil {
			t.Errorf("accepted query %+v", query)
		}
	}
	for _, request := range []CommentRequest{{}, {Text: " "}, {Text: strings.Repeat("я", 4001)}, {Text: string([]byte{0xff})},
		{Text: "Текст", Format: "unsupported"}, {Text: "Текст", ReplyTo: "other/comments"}} {
		if _, err := client.SendComment(context.Background(), "post-1", request); err == nil {
			t.Errorf("accepted invalid request")
		}
		if err := client.EditComment(context.Background(), "post-1", "comment-2", request); err == nil {
			t.Errorf("accepted invalid edit")
		}
	}
	if _, err := client.GetComment(context.Background(), "post-1", "../comment"); err == nil {
		t.Fatal("accepted traversal comment ID")
	}
	if _, err := client.SendComment(context.Background(), "../post", CommentRequest{Text: "Текст"}); err == nil {
		t.Fatal("accepted traversal post ID")
	}
	if err := client.DeleteComment(context.Background(), "post-1", "bad?id"); err == nil {
		t.Fatal("accepted invalid delete ID")
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("invalid inputs made %d requests", transport.calls.Load())
	}
}

func TestSendAndEditCommentUseOnlyDocumentedTextAndReplyFields(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/messages/post-1/comments" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 3 || string(body["format"]) != `"markdown"` || string(body["link"]) != `{"type":"reply","mid":"comment-1"}` {
			t.Errorf("unexpected comment body=%s", body)
		}
		var text string
		if err := json.Unmarshal(body["text"], &text); err != nil || text != strings.Repeat("я", 4000) {
			t.Error("comment Unicode bound not preserved")
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.RawQuery != "" {
				t.Error("send has unexpected query")
			}
			_, _ = io.WriteString(w, `{"message":`+sampleComment+`}`)
		case http.MethodPut:
			if r.URL.Query().Get("comment_id") != "comment-2" || len(r.URL.Query()) != 1 {
				t.Error("edit does not address comment through query")
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Error("unexpected mutation method")
		}
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	input := CommentRequest{Text: strings.Repeat("я", 4000), Format: FormatMarkdown, ReplyTo: "comment-1"}
	message, err := client.SendComment(context.Background(), "post-1", input)
	if err != nil || message.MessageID != "comment-2" {
		t.Fatalf("send=%+v error=%v", message, err)
	}
	if err := client.EditComment(context.Background(), "post-1", "comment-2", input); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected retries")
	}
}

func TestDeleteCommentUsesDocumentedEndpointAndPreservesOperationFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/messages/post-1/comments" || r.URL.Query().Get("comment_id") != "comment-2" || r.ContentLength != 0 {
			t.Errorf("unexpected delete %s %s", r.Method, r.URL.String())
		}
		_, _ = io.WriteString(w, `{"success":false,"code":"access.denied","message":"Not allowed"}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	err := client.DeleteComment(context.Background(), "post-1", "comment-2")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "access.denied" || apiErr.StatusCode != 200 {
		t.Fatalf("delete error=%v", err)
	}
}

func TestCommentMutationDistinguishesUnknownAcknowledgementFromExplicitRejection(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, test := range []struct {
			body     string
			rejected bool
		}{
			{`{}`, false},
			{`{"success":null}`, false},
			{`{"success":false,"code":"access.denied"}`, true},
		} {
			t.Run(method+test.body, func(t *testing.T) {
				t.Parallel()
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != method {
						t.Errorf("method=%s", r.Method)
					}
					_, _ = io.WriteString(w, test.body)
				}))
				defer server.Close()
				client := mustClient(t, server.URL, "token", server.Client())
				var err error
				if method == http.MethodPut {
					err = client.EditComment(context.Background(), "post-1", "comment-2", CommentRequest{Text: "Edited"})
				} else {
					err = client.DeleteComment(context.Background(), "post-1", "comment-2")
				}
				var apiErr *Error
				if err == nil || errors.As(err, &apiErr) != test.rejected || calls.Load() != 1 {
					t.Fatalf("rejected=%v error=%v calls=%d", test.rejected, err, calls.Load())
				}
				if test.rejected && (apiErr.StatusCode != 200 || apiErr.Code != "access.denied") {
					t.Fatalf("explicit rejection lost: %+v", apiErr)
				}
			})
		}
	}
}

func TestCommentsPreserveProviderRejectionsAndNeverRetryAmbiguousWrites(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"forbidden", 403, `{"code":"access.denied","message":"Denied"}`, "access.denied"},
		{"edit deadline", 422, `{"code":"comments.timeout_exceeded","message":"Too old"}`, "comments.timeout_exceeded"},
		{"rate limit", 429, `{"code":"rate.limit","message":"Slow down"}`, "rate.limit"},
		{"server error", 503, `{"code":"service.unavailable","message":"Unavailable"}`, "service.unavailable"},
		{"operation rejected", 200, `{"success":false,"code":"comments.disabled","message":"Disabled"}`, "comments.disabled"},
		{"malformed success", 200, `{"message":`, ""},
		{"missing successful message", 200, `{"success":true}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := mustClient(t, server.URL, "token", server.Client())
			_, err := client.SendComment(context.Background(), "post-1", CommentRequest{Text: "Текст"})
			if err == nil {
				t.Fatal("failure treated as success")
			}
			if test.code != "" {
				var apiErr *Error
				if !errors.As(err, &apiErr) || apiErr.Code != test.code || apiErr.StatusCode != test.status {
					t.Fatalf("upstream error lost: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("ambiguous/rejected send was retried")
			}
		})
	}
}

func TestGetCommentsRejectsUntrustedPageAssociationsAndShapes(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{}`, `{"messages":null}`, `{"messages":{}}`,
		`{"messages":[` + sampleComment + `,` + sampleComment + `]}`,
		`{"messages":[` + strings.Replace(sampleComment, `"post-1"`, `"other-post"`, 1) + `]}`,
		`{"messages":[` + strings.Replace(sampleComment, `"channel"`, `"chat"`, 1) + `]}`,
		`{"messages":[` + strings.Replace(sampleComment, `1791129600123`, `0`, 1) + `]}`,
		`{"messages":[` + strings.Replace(sampleComment, `"user_id":9007199254740993`, `"user_id":null`, 1) + `]}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			client := mustClient(t, server.URL, "token", server.Client())
			if _, err := client.GetComments(context.Background(), "post-1", CommentsQuery{Count: 2}); err == nil {
				t.Fatal("accepted malformed, duplicate or misassociated page")
			}
		})
	}
}

func TestCommentSingleReadChecksIdentityAndDoesNotTreat404AsDeletion(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/messages/post-1/comments/missing" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"code":"not.found","message":"Missing or inaccessible"}`)
			return
		}
		_, _ = io.WriteString(w, sampleComment)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	if _, err := client.GetComment(context.Background(), "post-1", "comment-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetComment(context.Background(), "post-1", "other-comment"); err == nil {
		t.Fatal("accepted wrong comment identity")
	}
	_, err := client.GetComment(context.Background(), "post-1", "missing")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("404 hidden: %v", err)
	}
}

func TestDecodeCommentNeverInfersPostFromReplyAndRejectsForward(t *testing.T) {
	t.Parallel()
	withoutRoot := strings.Replace(sampleComment, `,"post_id":"post-1"`, "", 1)
	message, err := DecodeCommentMessage(json.RawMessage(withoutRoot))
	if err != nil || message.PostID != "" || message.ReplyTo != "comment-1" {
		t.Fatalf("inferred root from reply: %+v, error=%v", message, err)
	}
	for _, raw := range []string{
		strings.Replace(sampleComment, `"type":"reply"`, `"type":"forward"`, 1),
		strings.Replace(sampleComment, `"post_id":"post-1"`, `"post_id":"bad/post"`, 1),
		strings.Replace(sampleComment, `"chat_id":-9007199254740993`, `"chat_id":"000"`, 1),
		strings.Replace(sampleComment, `"text":"Спасибо"`, `"text":"Спасибо","attachments":[{"type":"image"}]`, 1),
		sampleComment + `{}`,
	} {
		if _, err := DecodeCommentMessage(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted malformed or unsupported comment")
		}
	}
}

func TestCommentPermissionsDoNotInferChannelEditDeleteFromWrite(t *testing.T) {
	t.Parallel()
	for _, write := range []Permission{PermissionWrite, "post_edit_delete_message"} {
		membership := Membership{IsAdmin: true, Permissions: []Permission{PermissionReadAllMessages, write}}
		if !membership.HasCommentPermission(PermissionWrite) || !membership.HasCommentPermission(PermissionReadAllMessages) ||
			membership.HasCommentPermission(PermissionEdit) || membership.HasCommentPermission(PermissionDelete) {
			t.Fatalf("write grants unrelated channel rights: %+v", membership)
		}
	}
	for _, permissions := range [][]Permission{{PermissionEdit, PermissionDelete}, {"edit_message", "delete_message"}} {
		membership := Membership{Permissions: permissions}
		if !membership.HasCommentPermission(PermissionEdit) || !membership.HasCommentPermission(PermissionDelete) {
			t.Fatalf("lost explicit rights: %v", permissions)
		}
	}
}
