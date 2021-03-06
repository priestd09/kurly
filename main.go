package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aki237/nscjar"
	"github.com/alsm/ioprogress"
	"github.com/davidjpeacock/cli"
)

var (
	client   = http.Client{}
	Status   = log.New(os.Stderr, "*", 0)
	Incoming io.Writer
	Outgoing io.Writer
)

type LogWriter struct {
	*log.Logger
}

func (l *LogWriter) Write(p []byte) (n int, err error) {
	l.Print(string(p))
	return len(p), nil
}

func init() {
	cli.VersionFlag = cli.BoolFlag{
		Name:  "version, V",
		Usage: "print the version",
	}
	Incoming = &LogWriter{Logger: log.New(ioutil.Discard, "< ", 0)}
	Outgoing = &LogWriter{Logger: log.New(ioutil.Discard, "> ", 0)}
}

var body io.Reader

func main() {
	var target string
	var opts Options

	app := cli.NewApp()
	app.Name = "kurly"
	app.Usage = "[options] URL"
	app.Version = "1.1.0"
	opts.getOptions(app)

	app.Action = func(c *cli.Context) error {
		var remote *url.URL
		var err error

		client.CheckRedirect = opts.checkRedirect
		if opts.insecure {
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
		}
		opts.headers = c.StringSlice("header")
		opts.user = c.String("user")
		opts.dataAscii = c.StringSlice("data")
		opts.dataAscii = append(opts.dataAscii, c.StringSlice("data-ascii")...)
		opts.dataBinary = c.StringSlice("data-binary")
		opts.dataRaw = c.StringSlice("data-raw")
		opts.dataURLEncode = c.StringSlice("data-urlencode")
		opts.form = c.StringSlice("form")

		opts.ProcessData()
		d, err := opts.ProcessFormData()
		if err != nil {
			return err
		}
		if d != nil && len(opts.data) > 0 {
			Status.Fatalf("Error: unable to create http request; %s\n",
				"only one type of body can be accepted : either multipart form or url encoded values")
		}

		if c.NArg() == 0 {
			cli.ShowAppHelp(c)
			os.Exit(0)
		}

		if opts.verbose {
			Incoming.(*LogWriter).SetOutput(os.Stderr)
			Outgoing.(*LogWriter).SetOutput(os.Stderr)
		}

		if opts.head {
			opts.method = "HEAD"
			Incoming = io.MultiWriter(os.Stdout, Incoming.(*LogWriter))
		}

		if opts.maxTime > 0 {
			maxTime(opts.maxTime)
		}

		target = c.Args().Get(0)
		if remote, err = url.Parse(target); err != nil {
			Status.Fatalf("Error: %s does not parse correctly as a URL\n", target)
		}
		if remote.Scheme == "" {
			remote.Scheme = "http"
			remote, _ = url.Parse(remote.String())
		}

		if opts.remoteName {
			opts.outputFilename = path.Base(target)
		}

		outputFile := opts.openOutputFile()

		continueAtInt := uint64(0)
		if opts.continueAt != "" {
			if opts.continueAt == "-" {
				fileInfo, err := outputFile.Stat()
				if err != nil {
					Status.Fatalf("Error: unable to set content range automatically from file; %s\n", err)
				}
				continueAtInt = uint64(fileInfo.Size())
			} else {
				continueAtInt, err = strconv.ParseUint(opts.continueAt, 10, 64)
				if err != nil {
					Status.Fatalf("Error: unable to create http request; %s\n",
						"expected a valid positive number for continue-at option")
				}
			}
		}

		if opts.fileUpload != "" {
			opts.uploadFile()
		}

		if len(opts.data) > 0 || len(d) > 0 {
			var data bytes.Buffer
			opts.method = "POST"

			header := ""

			if len(opts.data) > 0 {
				header = "Content-Type: application/x-www-form-urlencoded"
				for i, d := range opts.data {
					data.WriteString(d)
					if i < len(opts.data)-1 {
						data.WriteRune('&')
					}
				}
			}

			if len(d) > 0 {
				w := multipart.NewWriter(&data)
				for key, field := range d {
					err := writeToMultipart(w, key, field)
					if err != nil {
						Status.Fatalf("Error: unable to create http request; %s\n", err)
					}
				}
				w.Close()
				header = "Content-Type: " + w.FormDataContentType()
			}

			opts.headers = append(opts.headers, header)
			body = &data
		}

		req, err := http.NewRequest(opts.method, target, body)
		if err != nil {
			Status.Fatalf("Error: unable to create http %s request; %s\n", opts.method, err)
		}

		if opts.verbose {
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), NewClientTraceForRequest(req)))
		}

		// Seek to given offset of the file and set the "Range" header
		if continueAtInt > 0 {
			if opts.outputFilename != "" {
				_, err = outputFile.Seek(int64(continueAtInt), 0)
				if err != nil {
					Status.Fatalf("Error: seek in the given output file; %s\n", err)
				}
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", continueAtInt))
		}
		req.Header.Set("User-Agent", opts.agent)
		if opts.user != "" {
			req.Header.Set("Authorization", "Basic "+encodeToBase64(opts.user))
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Host", remote.Host)
		if body != nil {
			switch b := body.(type) {
			case *os.File:
				fi, err := b.Stat()
				if err != nil {
					Status.Fatalf("Unable to get file stats for %v\n", opts.fileUpload)
				}
				req.ContentLength = fi.Size()
				req.Header.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
			case *ioprogress.Reader:
				req.ContentLength = b.Size
				req.Header.Set("Content-Length", strconv.FormatInt(b.Size, 10))
			case *bytes.Buffer:
				req.Header.Set("Content-Length", strconv.FormatInt(int64(b.Len()), 10))
			}
		}
		setHeaders(req, opts.headers)
		setCookieHeader(req, opts.cookie)

		resp, err := client.Do(req)
		if err != nil {
			Status.Fatalf("Error: Unable to get URL; %s\n", err)
		}
		defer resp.Body.Close()

		if continueAtInt > 0 && resp.StatusCode == 416 {
			Status.Fatalf("Error: Unable to get URL; %s\n", "Either the server doesn't support ranges or an invalid range is passed")
		}

		fmt.Fprintf(Incoming, "%s %s\n", resp.Proto, resp.Status)

		for k, v := range resp.Header {
			fmt.Fprintln(Incoming, k, v)
		}

		if !opts.head {
			if !opts.silent {
				progressR := &ioprogress.Reader{
					Reader: resp.Body,
					Size:   resp.ContentLength,
					DrawFunc: ioprogress.DrawTerminalf(os.Stderr, func(progress, total int64) string {
						return fmt.Sprintf(
							"%s %s",
							(ioprogress.DrawTextFormatBarWithIndicator(40, '<'))(progress, total),
							ioprogress.DrawTextFormatBytes(progress, total))
					}),
				}
				if _, err = io.Copy(outputFile, progressR); err != nil {
					Status.Fatalf("Error: Failed to copy URL content; %s\n", err)
				}
			}
			if opts.silent {
				if _, err = io.Copy(outputFile, resp.Body); err != nil {
					Status.Fatalf("Error: Failed to copy URL content; %s\n", err)
				}
			}
		}

		if opts.outputFilename != "" {
			outputFile.Close()
		}

		if rTime := resp.Header.Get("Last-Modified"); opts.remoteTime && rTime != "" {
			if t, err := time.Parse("Mon, 02 Jan 2006 15:04:05 MST", rTime); err == nil {
				os.Chtimes(opts.outputFilename, t, t)
			}
		}

		if opts.cookieJar != "" && len(resp.Cookies()) > 0 {
			cookies := resp.Cookies()
			for _, val := range cookies {
				if val.Domain == "" {
					u, err := resp.Location()
					if err != nil {
						u = req.URL
					}

					val.Domain = u.Hostname()
				}
			}
			saveCookies(cookies, opts.cookieJar)
		}
		return nil
	}

	app.Run(os.Args)
}

func setHeaders(r *http.Request, h []string) {
	for _, header := range h {
		hParts := strings.Split(header, ": ")
		switch len(hParts) {
		case 0:
			//surely not
		case 1:
			//must be an empty Header or a delete
			switch {
			case strings.HasSuffix(header, ";"):
				r.Header.Set(strings.TrimSuffix(header, ";"), "")
			case strings.HasSuffix(header, ":"):
				r.Header.Del(strings.TrimSuffix(header, ":"))
			default:
			}
		case 2:
			//standard expected
			r.Header.Set(hParts[0], hParts[1])
		default:
			//more than expected, use first element as Header name
			//and rejoin the rest as header content
			r.Header.Set(hParts[0], strings.Join(hParts[1:], ": "))
		}
	}
}

func writeToMultipart(w *multipart.Writer, key string, field Field) error {
	if field.IsFile {
		file, err := os.Open(field.Value)
		if err != nil {
			return err
		}
		defer file.Close()

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
				key, field.Filealias))
		h.Set("Content-Type", "application/octet-stream")
		if field.Type != "" {
			h.Set("Content-Type", field.Type)
		}

		fw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		if _, err = io.Copy(fw, file); err != nil {
			return err
		}
	} else {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="%s"`, key))
		if field.Type != "" {
			h.Set("Content-Type", field.Type)
		}
		wr, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = wr.Write([]byte(field.Value))
		if err != nil {
			return err
		}

	}
	return nil
}

func setCookieHeader(r *http.Request, arg string) {
	// according to cURL man pages and operations, if the cookie string passed has
	// a "=" in it, it means that is a valid cookie. Else it will search for the
	// cookie jar file of the name and tries to read cookies from it
	if strings.Contains(arg, "=") {
		r.Header.Set("Cookie", arg)
		return
	}

	// If the specified cookie string is invalid and the the file of the same name
	// doesn't exist, cURL doesn't throw any error, rather it just sets the cookie
	// to null
	cookieFile, err := os.Open(arg)
	if err != nil {
		return
	}

	p := nscjar.Parser{}

	cookies, err := p.Unmarshal(cookieFile)
	if err != nil {
		return
	}

	// reference time for checking the expiry of a cookie from the file
	t := time.Now()

	for _, val := range cookies {
		// Skip if cookie is secure and request isn't
		if val.Secure && r.URL.Scheme != "https" {
			continue
		}

		// Skip if the cookie has expired
		if val.Expires.Unix() <= t.Unix() {
			continue
		}

		host := r.URL.Hostname()
		// Domain matching (according to rfc6265::Section-5.1.3)
		if strings.ToLower(val.Domain) != strings.ToLower(host) {
			suffix := val.Domain
			if !strings.HasPrefix(val.Domain, ".") {
				suffix = "." + suffix
			}
			if !strings.HasSuffix(host, suffix) {
				continue
			}
		}

		// Path matching (according to rfc6265::Section-5.1.4)
		uri, _ := url.Parse("http://" + val.Domain + val.Path)
		if strings.HasPrefix(r.URL.Path, uri.Path) && r.URL.Path != uri.Path {
			continue
		}

		r.AddCookie(val)
	}
}

func saveCookies(cs []*http.Cookie, filename string) {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning : unable to save the cookie to the file : %s\n", err)
		return
	}

	jar := nscjar.NewCookieJar()

	// get the already stored cookies if exists in file.
	p := nscjar.Parser{}
	if cookies, err := p.Unmarshal(f); err == nil {
		jar.AddCookies(cookies...)
	}

	jar.AddCookies(cs...)

	_, err = f.Seek(0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning : unable to save the cookie to the file : %s\n", err)
		return
	}

	err = jar.Marshal(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning : unable to save the cookie to the file : %s\n", err)
	}
	err = f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning : unable to save the cookie to the file : %s\n", err)
	}
}

func maxTime(maxTime uint) {
	go func() {
		<-time.After(time.Duration(maxTime) * time.Second)
		Status.Fatalf("Error: Maximum operation time of %d seconds expired, aborting\n", maxTime)
	}()
}

func encodeToBase64(a string) string {
	return base64.StdEncoding.EncodeToString([]byte(a))
}
