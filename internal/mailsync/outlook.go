package mailsync

import (
	"context"
	"net/url"
	"strings"
	"time"
)

const graphBase = "https://graph.microsoft.com/v1.0/"

var graphHeaders = map[string]string{"Prefer": `IdType="ImmutableId", odata.maxpagesize=100`}

type Outlook struct{ API *API }

func (o Outlook) Identity(ctx context.Context) (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	e := o.API.json(ctx, graphBase+"me?$select=id", &p, nil)
	if e != nil {
		return "", e
	}
	if p.ID == "" {
		return "", ErrProvider
	}
	return p.ID, nil
}
func deltaURL(s string) bool {
	u, e := url.Parse(s)
	if len(s) > 16384 {
		return false
	}
	if e != nil || u.Scheme != "https" || u.Host != "graph.microsoft.com" || u.User != nil || u.Fragment != "" || !strings.HasPrefix(strings.ToLower(u.Path), "/v1.0/me/mailfolders") {
		return false
	}
	path := strings.ToLower(u.Path)
	return strings.HasSuffix(path, "/messages/delta") || strings.HasSuffix(path, "/messages/microsoft.graph.delta")
}
func (o Outlook) Page(ctx context.Context, c Connection, cursor Cursor) (Page, error) {
	endpoint := cursor.DeltaURL
	if endpoint == "" {
		folder := c.Folder
		if folder == "" {
			folder = "inbox"
		}
		query := url.Values{"$select": {"id,from,receivedDateTime"}, "$filter": {"receivedDateTime ge " + c.Since.UTC().Format(time.RFC3339)}, "changeType": {"created"}}
		endpoint = graphBase + "me/mailFolders/" + url.PathEscape(folder) + "/messages/delta?" + query.Encode()
	}
	if !deltaURL(endpoint) {
		return Page{}, ErrInvalid
	}
	var body struct {
		Next   string `json:"@odata.nextLink"`
		Delta  string `json:"@odata.deltaLink"`
		Values []struct {
			ID      string `json:"id"`
			Removed any    `json:"@removed"`
			From    struct {
				Address struct {
					Value string `json:"address"`
				} `json:"emailAddress"`
			} `json:"from"`
			Date time.Time `json:"receivedDateTime"`
		} `json:"value"`
	}
	if e := o.API.json(ctx, endpoint, &body, graphHeaders); e != nil {
		return Page{}, e
	}
	out := Page{Next: Cursor{Phase: "outlook_delta"}}
	backfill := cursor.Phase == "" || cursor.Phase == "outlook_backfill"
	if backfill {
		out.Next.Phase = "outlook_backfill"
		out.Next.Seen = cursor.Seen + len(body.Values)
		if out.Next.Seen >= 5000 {
			return Page{}, ErrWindowTooBroad
		}
	}
	for _, m := range body.Values {
		if m.Removed != nil || !amazonSender(m.From.Address.Value) || m.Date.Before(c.Since) {
			continue
		}
		if !validProviderID(m.ID) {
			return Page{}, ErrProvider
		}
		out.IDs = append(out.IDs, m.ID)
	}
	if body.Next != "" {
		if body.Delta != "" || !deltaURL(body.Next) {
			return Page{}, ErrProvider
		}
		out.Next.DeltaURL = body.Next
	} else {
		if !deltaURL(body.Delta) {
			return Page{}, ErrProvider
		}
		out.Next.DeltaURL = body.Delta
		out.Complete = true
		out.Next.Phase = "outlook_delta"
		out.Next.Seen = 0
	}
	return out, nil
}
func (o Outlook) Message(ctx context.Context, c Connection, id string) ([]byte, error) {
	if !validProviderID(id) {
		return nil, ErrInvalid
	}
	b, _, e := o.API.get(ctx, graphBase+"me/messages/"+url.PathEscape(id)+"/$value", 2<<20, graphHeaders)
	return b, e
}
