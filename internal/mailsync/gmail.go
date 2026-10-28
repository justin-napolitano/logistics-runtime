package mailsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me/"

type Gmail struct{ API *API }

func (g Gmail) profile(ctx context.Context) (string, string, error) {
	var p struct {
		Email   string `json:"emailAddress"`
		History string `json:"historyId"`
	}
	err := g.API.json(ctx, gmailBase+"profile", &p, nil)
	if err != nil {
		return "", "", err
	}
	if p.Email == "" || p.History == "" {
		return "", "", ErrProvider
	}
	return strings.ToLower(p.Email), p.History, nil
}
func (g Gmail) Identity(ctx context.Context) (string, error) {
	id, _, err := g.profile(ctx)
	return id, err
}
func (g Gmail) Page(ctx context.Context, c Connection, cursor Cursor) (Page, error) {
	if cursor.Phase == "" {
		_, history, e := g.profile(ctx)
		if e != nil {
			return Page{}, e
		}
		cursor = Cursor{Phase: "gmail_list", HistoryID: history}
	}
	query := url.Values{"maxResults": {"100"}}
	if cursor.PageToken != "" {
		query.Set("pageToken", cursor.PageToken)
	}
	switch cursor.Phase {
	case "gmail_list":
		query.Set("q", "after:"+strconv.FormatInt(c.Since.Unix(), 10)+" from:(amazon.com)")
		if c.Folder != "" {
			query.Set("labelIds", c.Folder)
		}
		var page struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			Next string `json:"nextPageToken"`
		}
		if e := g.API.json(ctx, gmailBase+"messages?"+query.Encode(), &page, nil); e != nil {
			return Page{}, e
		}
		out := Page{Next: cursor}
		for _, m := range page.Messages {
			if !validProviderID(m.ID) {
				return Page{}, ErrProvider
			}
			out.IDs = append(out.IDs, m.ID)
		}
		out.Next.PageToken = page.Next
		if page.Next == "" {
			out.Next.Phase = "gmail_history"
		}
		return out, nil
	case "gmail_history":
		if cursor.HistoryID == "" {
			return Page{}, ErrInvalid
		}
		query.Set("startHistoryId", cursor.HistoryID)
		query.Set("historyTypes", "messageAdded")
		if c.Folder != "" {
			query.Set("labelId", c.Folder)
		}
		var page struct {
			History []struct {
				Added []struct {
					Message struct {
						ID string `json:"id"`
					} `json:"message"`
				} `json:"messagesAdded"`
			} `json:"history"`
			Next      string `json:"nextPageToken"`
			HistoryID string `json:"historyId"`
		}
		err := g.API.json(ctx, gmailBase+"history?"+query.Encode(), &page, nil)
		if errors.Is(err, ErrGone) {
			return Page{}, ErrCursorExpired
		}
		if err != nil {
			return Page{}, err
		}
		out := Page{Next: cursor}
		seen := map[string]bool{}
		for _, h := range page.History {
			for _, a := range h.Added {
				if !validProviderID(a.Message.ID) {
					return Page{}, ErrProvider
				}
				if !seen[a.Message.ID] {
					seen[a.Message.ID] = true
					out.IDs = append(out.IDs, a.Message.ID)
				}
			}
		}
		out.Next.PageToken = page.Next
		if page.Next == "" {
			if page.HistoryID == "" {
				return Page{}, ErrProvider
			}
			out.Next.HistoryID = page.HistoryID
			out.Complete = true
		}
		return out, nil
	default:
		return Page{}, ErrInvalid
	}
}
func validProviderID(s string) bool {
	return len(s) > 0 && len(s) <= 512 && !strings.ContainsAny(s, "\x00\r\n")
}
func amazonSender(s string) bool {
	a, e := mail.ParseAddress(s)
	if e != nil {
		return false
	}
	parts := strings.Split(strings.ToLower(a.Address), "@")
	return len(parts) == 2 && (parts[1] == "amazon.com" || strings.HasSuffix(parts[1], ".amazon.com"))
}
func (g Gmail) Message(ctx context.Context, c Connection, id string) ([]byte, error) {
	if !validProviderID(id) {
		return nil, ErrInvalid
	}
	endpoint := gmailBase + "messages/" + url.PathEscape(id)
	var meta struct {
		Date    string `json:"internalDate"`
		Payload struct {
			Headers []struct{ Name, Value string } `json:"headers"`
		} `json:"payload"`
	}
	if e := g.API.json(ctx, endpoint+"?format=metadata&metadataHeaders=From", &meta, nil); e != nil {
		return nil, e
	}
	from := ""
	for _, h := range meta.Payload.Headers {
		if strings.EqualFold(h.Name, "From") {
			from = h.Value
		}
	}
	if !amazonSender(from) {
		return nil, nil
	}
	ms, e := strconv.ParseInt(meta.Date, 10, 64)
	if e != nil {
		return nil, ErrProvider
	}
	if time.UnixMilli(ms).Before(c.Since) {
		return nil, nil
	}
	raw, _, e := g.API.get(ctx, endpoint+"?format=raw", 3<<20, nil)
	if e != nil {
		return nil, e
	}
	var body struct {
		Raw string `json:"raw"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Raw == "" {
		return nil, ErrProvider
	}
	data, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(body.Raw, "="))
	if e != nil {
		return nil, ErrProvider
	}
	if len(data) > 2<<20 {
		return nil, ErrTooLarge
	}
	return data, nil
}
