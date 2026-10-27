package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/postgres"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

func templateFile(path string) (shopping.SavedTemplate, error) {
	f, err := os.Open(path)
	if err != nil {
		return shopping.SavedTemplate{}, errors.New("cannot open template file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 128<<10+1))
	if err != nil || len(data) > 128<<10 {
		return shopping.SavedTemplate{}, shopping.ErrInvalid
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var t shopping.Template
	if err = dec.Decode(&t); err != nil {
		return shopping.SavedTemplate{}, shopping.ErrInvalid
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return shopping.SavedTemplate{}, shopping.ErrInvalid
	}
	return shopping.Clean(t)
}
func outputReview(b shopping.Basket, path string) error {
	if path == "" {
		return shopping.Render(os.Stdout, b)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create review file; choose a new output path")
	}
	defer f.Close()
	return shopping.Render(f, b)
}
func preview(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("usage: logistics basket-preview TEMPLATE.json CYCLE [NEW_OUTPUT.html]")
	}
	t, err := templateFile(args[0])
	if err != nil {
		return err
	}
	b, err := shopping.Prepare(context.Background(), t, args[1], nil, time.Now())
	if err != nil {
		return errors.New("preview requires planned_packages lines and a valid cycle; inventory_gap needs the database")
	}
	path := ""
	if len(args) == 3 {
		path = args[2]
	}
	return outputReview(b, path)
}
func shoppingCommand(ctx context.Context, s *postgres.Store, cmd string, args []string) error {
	switch cmd {
	case "save-template":
		if len(args) != 1 {
			return errors.New("usage: logistics save-template TEMPLATE.json")
		}
		t, err := templateFile(args[0])
		if err != nil {
			return err
		}
		if err = s.SaveTemplate(ctx, t); err != nil {
			return errors.New("template save failed; check shopping migrations")
		}
		return json.NewEncoder(os.Stdout).Encode(t)
	case "prepare-basket":
		if len(args) != 2 {
			return errors.New("usage: logistics prepare-basket TEMPLATE_ID CYCLE")
		}
		id := shopping.Digest([]byte(args[0] + "\x00" + args[1]))
		if b, err := s.Basket(ctx, id); err == nil {
			return json.NewEncoder(os.Stdout).Encode(b)
		}
		t, err := s.Template(ctx, args[0])
		if err != nil {
			return errors.New("template unavailable")
		}
		b, err := shopping.Prepare(ctx, t, args[1], s, time.Now())
		if err != nil {
			return errors.New("basket preparation failed; check quantities and inventory units")
		}
		b, err = s.SaveBasket(ctx, b)
		if err != nil {
			return errors.New("basket save failed")
		}
		return json.NewEncoder(os.Stdout).Encode(b)
	case "basket-export":
		if len(args) != 2 {
			return errors.New("usage: logistics basket-export BASKET_ID NEW_OUTPUT.html")
		}
		b, err := s.Basket(ctx, args[0])
		if err != nil {
			return errors.New("basket unavailable")
		}
		return outputReview(b, args[1])
	case "import-mail":
		if len(args) < 2 || len(args) > 1001 {
			return errors.New("usage: logistics import-mail ACCOUNT FILE.eml [FILE.eml ...]")
		}
		return importFiles(ctx, s, args[0], args[1:])
	case "import-mail-dir":
		if len(args) != 2 {
			return errors.New("usage: logistics import-mail-dir ACCOUNT DIRECTORY")
		}
		dir, err := os.Open(args[1])
		if err != nil {
			return errors.New("cannot open mail directory")
		}
		defer dir.Close()
		entries, err := dir.ReadDir(1001)
		if err != nil && err != io.EOF {
			return errors.New("cannot read mail directory")
		}
		if len(entries) > 1000 {
			return errors.New("mail directory exceeds 1000 entries; split the import")
		}
		files := []string{}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".eml") {
				files = append(files, filepath.Join(args[1], e.Name()))
			}
		}
		return importFiles(ctx, s, args[0], files)
	case "orders":
		if len(args) > 1 {
			return errors.New("usage: logistics orders [ORDER_ID]")
		}
		if len(args) == 1 {
			o, e := s.Order(ctx, args[0])
			if e != nil {
				return errors.New("order unavailable")
			}
			return json.NewEncoder(os.Stdout).Encode(o)
		}
		out, e := s.Orders(ctx, 100, 0)
		if e != nil {
			return errors.New("order list unavailable")
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "mail-exceptions":
		if len(args) != 0 {
			return errors.New("usage: logistics mail-exceptions")
		}
		out, e := s.MailExceptions(ctx, 100, 0)
		if e != nil {
			return errors.New("exception list unavailable")
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	return errors.New("unknown shopping command")
}
func importFiles(ctx context.Context, s *postgres.Store, account string, files []string) error {
	for _, path := range files {
		f, e := os.Open(path)
		if e != nil {
			return errors.New("cannot open mail file")
		}
		m, e := ordermail.Parse(f)
		f.Close()
		if e != nil {
			return errors.New("invalid or oversized mail file")
		}
		result, e := s.ImportMail(ctx, account, m)
		if e != nil {
			return errors.New("mail import failed; source identity conflict or storage unavailable")
		}
		if e = json.NewEncoder(os.Stdout).Encode(result); e != nil {
			return e
		}
	}
	return nil
}
