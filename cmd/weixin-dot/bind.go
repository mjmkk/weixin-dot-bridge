// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mjmkk/weixin-dot-bridge/internal/weixin"
	qrcode "github.com/skip2/go-qrcode"
)

func runBind(args []string) error {
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("bind", flag.ContinueOnError)
	credentials := fs.String("credentials", filepath.Join(home, ".config/weixin-dot/weixin.auth.json"), "private Weixin credential destination")
	sessionPath := fs.String("session", filepath.Join(home, ".local/state/weixin-dot/qr-session.secret.json"), "private QR session for resumption")
	pngPath := fs.String("qr-image", filepath.Join(home, ".local/state/weixin-dot/weixin-login.png"), "QR image path")
	resume := fs.Bool("resume", false, "resume the pending QR without creating a new binding")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := map[string]bool{}
	for _, path := range []string{*credentials, *sessionPath, *pngPath} {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if paths[absolute] {
			return errors.New("binding credentials, session and QR image must use separate paths")
		}
		paths[absolute] = true
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("another binding process owns these login files")
		}
	}
	if _, err := os.Stat(*credentials); err == nil {
		return errors.New("Weixin credential file already exists; use bridge or explicitly manage re-binding")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := weixin.NewClient(weixin.Config{}, weixin.Credentials{})
	if err != nil {
		return err
	}
	var session *weixin.QRSession
	if *resume {
		info, err := os.Stat(*sessionPath)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("QR session must be private")
		}
		data, err := os.ReadFile(*sessionPath)
		if err != nil {
			return err
		}
		session = &weixin.QRSession{}
		if json.Unmarshal(data, session) != nil {
			return errors.New("invalid QR session")
		}
	} else {
		session, err = c.StartQR(ctx, nil)
		if err != nil {
			return err
		}
		if err = save(*sessionPath, session); err != nil {
			return err
		}
	}
	png, err := qrcode.Encode(session.DisplayURL, qrcode.Medium, 384)
	if err != nil {
		return errors.New("cannot render Weixin QR")
	}
	if err = saveBytes(*pngPath, png); err != nil {
		return err
	}
	emit(map[string]any{"status": "awaiting_scan", "qr_image": *pngPath})
	reader := bufio.NewReader(os.Stdin)
	last := weixin.QRState("")
	code := ""
	for ctx.Err() == nil {
		status, err := c.PollQR(ctx, session, code)
		code = ""
		if err != nil {
			return err
		}
		if status.State != last {
			emit(map[string]any{"status": status.State})
			last = status.State
		}
		switch status.State {
		case weixin.QRConfirmed:
			if status.Credentials == nil {
				return errors.New("QR confirmation has no credentials")
			}
			if err = save(*credentials, status.Credentials); err != nil {
				return err
			}
			emit(map[string]any{"status": "bound", "credentials_file": *credentials, "peer": status.Credentials.UserID, "bot": status.Credentials.BotID})
			return nil
		case weixin.QRExpired:
			return errors.New("QR expired; create a fresh QR to continue")
		case weixin.QRCodeBlocked:
			return errors.New("native verification blocked; user must recover in Weixin")
		case weixin.QRAlreadyBound:
			return errors.New("already bound elsewhere; no local credential obtained")
		case weixin.QRNeedsCode:
			fmt.Fprintln(os.Stderr, "Enter the verification code shown by Weixin on stdin.")
			line, err := readVerification(ctx, reader)
			if err != nil {
				return errors.New("native verification requires user input")
			}
			code = strings.TrimSpace(line)
			if code == "" {
				return errors.New("verification code is empty")
			}
		case weixin.QRRedirect:
			if err = save(*sessionPath, session); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

// The API's native challenge is answered by the user. In the automated host
// stdin is a private pipe; refuse a terminal whose echo would reveal the code.
func readVerification(ctx context.Context, reader *bufio.Reader) (string, error) {
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", errors.New("native code requires a private stdin pipe; resume the saved QR")
	}
	type result struct {
		line string
		err  error
	}
	ready := make(chan result, 1)
	go func() { line, err := reader.ReadString('\n'); ready <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ready:
		return r.line, r.err
	}
}
