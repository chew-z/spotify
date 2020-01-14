// Package spotify provides utilties for interfacing
// with Spotify's Web API.
package spotify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
)

// Version is the version of this library.
const Version = "1.0.0"

const (
	// DateLayout can be used with time.Parse to create time.Time values
	// from Spotify date strings.  For example, PrivateUser.Birthdate
	// uses this format.
	DateLayout = "2006-01-02"
	// TimestampLayout can be used with time.Parse to create time.Time
	// values from SpotifyTimestamp strings.  It is an ISO 8601 UTC timestamp
	// with a zero offset.  For example, PlaylistTrack's AddedAt field uses
	// this format.
	TimestampLayout = "2006-01-02T15:04:05Z"

	// defaultRetryDurationS helps us fix an apparent server bug whereby we will
	// be told to retry but not be given a wait-interval.
	defaultRetryDuration = time.Second * 5

	// rateLimitExceededStatusCode is the code that the server returns when our
	// request frequency is too high.
	rateLimitExceededStatusCode = 429
)

const baseAddress = "https://api.spotify.com/v1/"

// Client is a client for working with the Spotify Web API.
// To create an authenticated client, use the `Authenticator.NewClient` method.
type Client struct {
	http    *http.Client
	baseURL string

	AutoRetry bool
}

// URI identifies an artist, album, track, or category.  For example,
// spotify:track:6rqhFgbbKwnb9MLmUQDhG6
type URI string

// ID is a base-62 identifier for an artist, track, album, etc.
// It can be found at the end of a spotify.URI.
type ID string

type cachedResponse struct {
	Etag   string
	Result *[]byte
}

var kaszka = cache.New(20*time.Minute, 3*time.Minute)

func (id *ID) String() string {
	return string(*id)
}

// Followers contains information about the number of people following a
// particular artist or playlist.
type Followers struct {
	// The total number of followers.
	Count uint `json:"total"`
	// A link to the Web API endpoint providing full details of the followers,
	// or the empty string if this data is not available.
	Endpoint string `json:"href"`
}

// Image identifies an image associated with an item.
type Image struct {
	// The image height, in pixels.
	Height int `json:"height"`
	// The image width, in pixels.
	Width int `json:"width"`
	// The source URL of the image.
	URL string `json:"url"`
}

// Download downloads the image and writes its data to the specified io.Writer.
func (i Image) Download(dst io.Writer) error {
	resp, err := http.Get(i.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// TODO: get Content-Type from header?
	if resp.StatusCode != http.StatusOK {
		return errors.New("Couldn't download image - HTTP" + strconv.Itoa(resp.StatusCode))
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

// Error represents an error returned by the Spotify Web API.
type Error struct {
	// A short description of the error.
	Message string `json:"message"`
	// The HTTP status code.
	Status int `json:"status"`
}

func (e Error) Error() string {
	return e.Message
}

// decodeError decodes an Error from an io.Reader.
func (c *Client) decodeError(resp *http.Response) error {
	responseBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if len(responseBody) == 0 {
		return fmt.Errorf("spotify: HTTP %d: %s (body empty)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	buf := bytes.NewBuffer(responseBody)

	var e struct {
		E Error `json:"error"`
	}
	err = json.NewDecoder(buf).Decode(&e)
	if err != nil {
		return fmt.Errorf("spotify: couldn't decode error: (%d) [%s]", len(responseBody), responseBody)
	}

	if e.E.Message == "" {
		// Some errors will result in there being a useful status-code but an
		// empty message, which will confuse the user (who only has access to
		// the message and not the code). An example of this is when we send
		// some of the arguments directly in the HTTP query and the URL ends-up
		// being too long.

		e.E.Message = fmt.Sprintf("spotify: unexpected HTTP %d: %s (empty error)",
			resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return e.E
}

// shouldRetry determines whether the status code indicates that the
// previous operation should be retried at a later time
func shouldRetry(status int) bool {
	return status == http.StatusAccepted || status == http.StatusTooManyRequests
}

// isFailure determines whether the code indicates failure
func isFailure(code int, validCodes []int) bool {
	for _, item := range validCodes {
		if item == code {
			return false
		}
	}
	return true
}

// `execute` executes a non-GET request. `needsStatus` describes other HTTP
// status codes that will be treated as success. Note that we allow all 200s
// even if there are additional success codes that represent success.
func (c *Client) execute(req *http.Request, result interface{}, needsStatus ...int) error {
	for {
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if c.AutoRetry && shouldRetry(resp.StatusCode) {
			time.Sleep(retryDuration(resp))
			continue
		}
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if (resp.StatusCode >= 300 ||
			resp.StatusCode < 200) &&
			isFailure(resp.StatusCode, needsStatus) {
			return c.decodeError(resp)
		}

		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				return err
			}
		}
		break
	}
	return nil
}

func retryDuration(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return defaultRetryDuration
	}
	seconds, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return defaultRetryDuration
	}
	return time.Duration(seconds) * time.Second
}

//
func (c *Client) get(url string, result interface{}) error {
	for {
		req, _ := http.NewRequest("GET", url, nil)
		var etag string
		if k, found := kaszka.Get(url); found {
			b := k.(*cachedResponse)
			etag = b.Etag
			if etag != "" {
				req.Header.Set("If-None-Match", etag)
			} else {
				body := ioutil.NopCloser(bytes.NewBuffer(*b.Result))
				err := json.NewDecoder(body).Decode(result)
				if err != nil {
					return err
				}
				log.Println("spotify: using cached response")
				break
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		// defer resp.Body.Close()
		if resp.StatusCode == rateLimitExceededStatusCode && c.AutoRetry {
			time.Sleep(retryDuration(resp))
			continue
		}
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
			return c.decodeError(resp)
		}
		if resp.StatusCode == http.StatusNotModified {
			log.Printf("spotify: response: %d", resp.StatusCode)
			if k, found := kaszka.Get(url); found {
				b := k.(*cachedResponse)
				resp.Body = ioutil.NopCloser(bytes.NewBuffer(*b.Result))
				err = json.NewDecoder(resp.Body).Decode(result)
				if err != nil {
					return err
				}
			}
			log.Println("spotify: using ETag response")
			break
		}
		if resp.StatusCode == http.StatusOK {
			log.Printf("spotify: response: %d", resp.StatusCode)
			bodyBytes, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close() //  must close
			resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
			err = json.NewDecoder(resp.Body).Decode(result)
			if err != nil {
				return err
			}
			// log.Printf("result: %v", result)
			resp.Body.Close() //  must close to reuse
			resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
			cacheResponse(resp, url, &bodyBytes) // cache response body
			break
		}

	}
	return nil
}

/*cacheResponse - is caching response body for url handling both
Cache-Control and Etag (Spotify is using one or the other)
Response is cached until expiration.
*/
func cacheResponse(res *http.Response, url string, body *[]byte) {
	var cR cachedResponse
	cc := res.Header.Get("Cache-Control")
	log.Printf("spotify: Cache-Control: %s", cc)
	var cci int
	if cc != "" {
		i := strings.Index(cc, "max-age=")
		if i != -1 {
			i += 8
			j := i + 1
			for ; j < len(cc); j++ {
				if cc[j] >= '0' && cc[j] <= '9' {
					continue
				}
				break
			}
			cci, _ = strconv.Atoi(cc[i:j])
		}
	}
	// Heuristics for guessing the expiration
	var expires string
	if cci == 0 {
		expires = res.Header.Get("Expires")
		log.Printf("spotify: Expires: %s", res.Header.Get("Expires"))
	}
	iee := cci == 0 && isEmptyExpires(expires)
	lm := res.Header.Get("Last-Modified")
	et := res.Header.Get("ETag")
	log.Printf("spotify: Last-Modified: %s", lm)
	log.Printf("spotify: ETag: %s", et)
	if lm == "" && et == "" && iee {
		return
	}
	// normalize to time.Duration for cache
	var ed time.Duration
	if !iee {
		if cci != 0 {
			ed = time.Duration(cci) * time.Second
		} else {
			d, err := time.Parse(time.RFC1123, expires)
			if err == nil {
				ed = d.Sub(time.Now())
			}
		}
	}
	log.Printf("Expires: %s", duration(ed))
	if et != "" {
		cR.Etag = et
	} else {
		cR.Etag = ""
		// cR.Etag = etag.Generate(string(*body), false) // If Spotify have not provided ETag make it yourself
		// log.Printf("Etag: %s", cR.Etag)
	}
	cR.Result = body
	if ed > 0.0 {
		kaszka.Set(url, &cR, ed)
	}
	return
}

func duration(d time.Duration) string {
	const (
		day  = time.Minute * 60 * 24
		year = 365 * day
	)
	if d < day {
		return d.String()
	}

	var b strings.Builder
	if d >= year {
		years := d / year
		fmt.Fprintf(&b, "%dy", years)
		d -= years * year
	}

	days := d / day
	d -= days * day
	fmt.Fprintf(&b, "%dd%s", days, d)

	return b.String()
}

// Options contains optional parameters that can be provided
// to various API calls.  Only the non-nil fields are used
// in queries.
type Options struct {
	// Country is an ISO 3166-1 alpha-2 country code.  Provide
	// this parameter if you want the list of returned items to
	// be relevant to a particular country.  If omitted, the
	// results will be relevant to all countries.
	Country *string
	// Limit is the maximum number of items to return.
	Limit *int
	// Offset is the index of the first item to return.  Use it
	// with Limit to get the next set of items.
	Offset *int
	// Timerange is the period of time from which to return results
	// in certain API calls. The three options are the following string
	// literals: "short", "medium", and "long"
	Timerange *string
}

// NewReleasesOpt is like NewReleases, but it accepts optional parameters
// for filtering the results.
func (c *Client) NewReleasesOpt(opt *Options) (albums *SimpleAlbumPage, err error) {
	spotifyURL := c.baseURL + "browse/new-releases"
	if opt != nil {
		v := url.Values{}
		if opt.Country != nil {
			v.Set("country", *opt.Country)
		}
		if opt.Limit != nil {
			v.Set("limit", strconv.Itoa(*opt.Limit))
		}
		if opt.Offset != nil {
			v.Set("offset", strconv.Itoa(*opt.Offset))
		}
		if params := v.Encode(); params != "" {
			spotifyURL += "?" + params
		}
	}

	var objmap map[string]*json.RawMessage
	err = c.get(spotifyURL, &objmap)
	if err != nil {
		return nil, err
	}

	var result SimpleAlbumPage
	err = json.Unmarshal(*objmap["albums"], &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// NewReleases gets a list of new album releases featured in Spotify.
// This call requires bearer authorization.
func (c *Client) NewReleases() (albums *SimpleAlbumPage, err error) {
	return c.NewReleasesOpt(nil)
}

func isEmptyExpires(expires string) bool {
	return expires == "" || expires == "-1" || expires == "0"
}
