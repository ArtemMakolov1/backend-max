package store

import "encoding/json"

// MarshalJSON keeps collection fields consistent with the public Post schema,
// including publications that have no gallery or link buttons. A shallow copy
// avoids changing the caller's snapshot while normalizing nil slices.
func (post Post) MarshalJSON() ([]byte, error) {
	type postJSON Post
	value := postJSON(post)
	if value.Attachments == nil {
		value.Attachments = []PostAttachment{}
	}
	if value.LinkButtons == nil {
		value.LinkButtons = []LinkButton{}
	}
	return json.Marshal(value)
}
