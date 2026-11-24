package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/qrcode"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// isTerminal reports whether w is a terminal (QR codes are drawn only there).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// writeQR draws a compact QR code for link when w is a terminal.
func writeQR(w io.Writer, link string) {
	if !isTerminal(w) {
		return
	}
	_ = qrcode.WriteTerminal(w, link, os.Getenv("NO_COLOR") == "")
}

func minutesLeft(until time.Time) int {
	return max(1, int(time.Until(until).Round(time.Minute).Minutes()))
}

func newPairCmd() *cobra.Command {
	var urlFlag, token, tokensFile string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Print a one-time sign-in link and QR code for a browser or phone",
		Long: "Ask the hub for a single-use sign-in link (valid 10 minutes). Open it on the device\n" +
			"to sign in, or scan the QR code with a phone. On the hub's machine this needs no\n" +
			"credential; elsewhere, sign in with `konsert login` first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t := resolveHub(urlFlag, token, tokensFile)
			var pc controller.PairingCode
			if err := newHubClient(t, auditSourceCLI).DoJSON(cmd.Context(), http.MethodPost, "/api/pairing-codes", struct{}{}, &pc); err != nil {
				return friendlyAPIError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSONOut(out, pc)
			}
			fmt.Fprintf(out, "Sign in: %s (%d min)\n", pc.URL, minutesLeft(pc.ExpiresAt))
			writeQR(out, pc.URL)
			return nil
		},
	}
	addClientFlags(cmd, &urlFlag, &token)
	cmd.Flags().StringVar(&tokensFile, "tokens-file", "", "The local hub's tokens file (default: found automatically)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON")
	return cmd
}

// auditSourceCLI labels CLI requests in the hub's audit log.
const auditSourceCLI = "cli"

type loginStart struct {
	Code      string    `json:"code"`
	SID       string    `json:"sid"`
	VerifyURL string    `json:"verify_url"`
	PollToken string    `json:"poll_token"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval"`
}

type loginPoll struct {
	Status string `json:"status"`
	Token  string `json:"token"`
	Device struct {
		SID  string `json:"sid"`
		Name string `json:"name"`
	} `json:"device"`
}

func defaultCLIName() string {
	host, _ := os.Hostname()
	if host == "" {
		return "konsert CLI"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return "konsert CLI on " + host
}

func newLoginCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "login <hub-url>",
		Short: "Sign this CLI in to a hub by approving it in a signed-in browser",
		Long: "Start a sign-in request on the hub, then approve it in a browser that is already\n" +
			"signed in (open the printed link or scan the QR code). The CLI saves its own device\n" +
			"credential in your user config directory and uses it for later commands.\n" +
			"On the hub's own machine no login is needed.",
		Example: "  konsert login https://desk.example.ts.net\n  konsert login desk:8420",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, err := normalizeHubURL(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runLogin(ctx, cmd.OutOrStdout(), hubURL, name, time.Sleep)
		},
	}
	cmd.Flags().StringVar(&name, "name", defaultCLIName(), "Device name shown in Settings → Devices")
	return cmd
}

// runLogin performs the device-code flow against hubURL and saves the credential.
func runLogin(ctx context.Context, out io.Writer, hubURL, name string, sleep func(time.Duration)) error {
	client := newHubClient(hubTarget{URL: hubURL}, auditSourceCLI)
	var start loginStart
	if err := client.DoJSON(ctx, http.MethodPost, "/api/auth/device-code", map[string]string{"name": name, "kind": "cli"}, &start); err != nil {
		return friendlyAPIError(err)
	}
	fmt.Fprintf(out, "Approve this sign-in in a browser signed in to %s:\n  %s\n", hubURL, start.VerifyURL)
	fmt.Fprintf(out, "Code: %s (expires in %d min)\n", start.Code, minutesLeft(start.ExpiresAt))
	writeQR(out, start.VerifyURL)
	fmt.Fprintln(out, "Waiting for approval...")
	interval := time.Duration(max(start.Interval, 1)) * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return errors.New("sign-in cancelled")
		}
		var p loginPoll
		err := client.DoJSON(ctx, http.MethodPost, "/api/auth/device-code/poll", map[string]string{"poll_token": start.PollToken}, &p)
		var apiErr *controller.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests:
			interval += time.Second
		case err != nil:
			if ctx.Err() != nil {
				return errors.New("sign-in cancelled")
			}
			return friendlyAPIError(err)
		}
		switch p.Status {
		case "approved":
			creds, path, err := loadCredentials()
			if err != nil {
				return err
			}
			creds.Hubs[hubURL] = savedCredential{Token: p.Token, DeviceSID: p.Device.SID, Name: p.Device.Name, SavedAt: time.Now().UTC()}
			creds.Current = hubURL
			if err := saveCredentials(creds, path); err != nil {
				return err
			}
			fmt.Fprintf(out, "Signed in to %s as %s (%s).\n", hubURL, p.Device.Name, p.Device.SID)
			return nil
		case "denied":
			return errors.New("the sign-in request was denied")
		case "expired", "redeemed", "unknown":
			return fmt.Errorf("the sign-in request %s; run konsert login again", map[string]string{
				"expired": "expired", "redeemed": "was already used", "unknown": "is no longer known to the hub"}[p.Status])
		}
		sleep(interval)
	}
}

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout [hub-url]",
		Short: "Sign this CLI out of a hub (revokes its device credential)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, path, err := loadCredentials()
			if err != nil {
				return err
			}
			hubURL := creds.Current
			if len(args) == 1 {
				if hubURL, err = normalizeHubURL(args[0]); err != nil {
					return err
				}
			}
			saved, ok := creds.Hubs[hubURL]
			if hubURL == "" || !ok {
				return errors.New("not signed in to that hub (see konsert login)")
			}
			out := cmd.OutOrStdout()
			client := newHubClient(hubTarget{URL: hubURL, Token: saved.Token}, auditSourceCLI)
			if err := client.DoJSON(cmd.Context(), http.MethodPost, "/api/auth/logout", struct{}{}, nil); err != nil {
				fmt.Fprintf(out, "warning: could not reach %s to revoke this device (%v); revoke %s in Settings → Devices\n", hubURL, err, saved.DeviceSID)
			}
			delete(creds.Hubs, hubURL)
			if creds.Current == hubURL {
				creds.Current = ""
			}
			if err := saveCredentials(creds, path); err != nil {
				return err
			}
			fmt.Fprintf(out, "Signed out of %s.\n", hubURL)
			return nil
		},
	}
	return cmd
}

// newRotateRecoveryCmd is `konsert token rotate recovery`.
func newRotateRecoveryCmd() *cobra.Command {
	var urlFlag, tokensFile string
	cmd := &cobra.Command{
		Use:   "recovery",
		Short: "Replace the hub's recovery key (run on the hub's machine)",
		Long: "Generate a new recovery key and store it in the hub's tokens file. A running hub\n" +
			"switches at once and the old key stops working. The key is never printed: it stays\n" +
			"in the tokens file (mode 0600) for emergencies. Signed-in devices are unaffected.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return rotateRecovery(cmd.Context(), cmd.OutOrStdout(), urlFlag, localTokensFile(tokensFile))
		},
	}
	cmd.Flags().StringVar(&urlFlag, "url", "", "URL of the hub on this machine (default: found automatically)")
	cmd.Flags().StringVar(&tokensFile, "tokens-file", "", "The hub's tokens file (default: found automatically)")
	return cmd
}

// rotateRecovery asks the running hub to rotate its recovery key (authenticated with
// the current key from the tokens file) or, when no hub is running, rewrites the file.
func rotateRecovery(ctx context.Context, out io.Writer, urlFlag, path string) error {
	saved, err := readTokensFile(path)
	if err != nil {
		return err
	}
	if saved.RecoveryKey == "" {
		return fmt.Errorf("no recovery key in %s (is this the hub's tokens file? pass --tokens-file)", path)
	}
	hubURL := urlFlag
	if hubURL == "" {
		if h := loadLocalHub(); h.URL != "" {
			hubURL = h.URL
		} else {
			hubURL = defaultHubURL
		}
	}
	if !isLoopbackURL(hubURL) {
		return errors.New("run this on the hub's machine: the recovery key is only sent to a local hub")
	}
	client := newHubClient(hubTarget{URL: hubURL, Token: saved.RecoveryKey}, auditSourceCLI)
	err = client.DoJSON(ctx, http.MethodPost, "/api/auth/recovery-key/rotate", struct{}{}, nil)
	var netErr net.Error
	var opErr *net.OpError
	switch {
	case err == nil:
		fmt.Fprintf(out, "Recovery key rotated; the hub saved the new key to its tokens file.\n")
		return nil
	case errors.As(err, &opErr) || errors.As(err, &netErr):
		k, genErr := auth.GenerateToken(32)
		if genErr != nil {
			return genErr
		}
		if err := rotateRecoveryKeyInFile(path, k); err != nil {
			return err
		}
		fmt.Fprintf(out, "No hub running at %s; rotated the recovery key in %s.\n", hubURL, path)
		return nil
	default:
		return friendlyAPIError(err)
	}
}
