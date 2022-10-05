package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/docopt/docopt-go"
	"github.com/motemen/go-pocket/api"
	"github.com/motemen/go-pocket/auth"
)

var version = "0.1"

var defaultItemTemplate = template.Must(template.New("item").Parse(
	"[{{.ItemID | printf \"%9d\"}}] ({{.TimeAdded.Format \"Mon, 02 Jan 2006 15:04:05 MST\"}}) {{.Title}}\n<{{.URL}}>",
))

var configDir string

func init() {
	usr, err := user.Current()
	if err != nil {
		panic(err)
	}

	configDir = filepath.Join(usr.HomeDir, ".config", "pocket")
	err = os.MkdirAll(configDir, 0777)
	if err != nil {
		panic(err)
	}
}

func CleanURL(url string) string {
	// HTTPS by default
	url = strings.ReplaceAll(url, "http://", "https://")
	// Remove some youtube params
	url = strings.TrimSuffix(url, "&a")
	url = strings.ReplaceAll(url, "feature=g-u", "")
	url = strings.ReplaceAll(url, "feature=youtu.be", "")
	url = strings.ReplaceAll(url, "feature=youtube_gdata", "")
	// Deduplicate any double ampersands
	amps := regexp.MustCompile("&+")
	url = amps.ReplaceAllString(url, "&")
	url = strings.TrimRight(url, "&")
	// Drop trailing slash
	url = strings.TrimRight(url, "/")

	return url
}

type Config struct {
	List    bool `docopt:"list"`
	Archive bool `docopt:"archive"`
	Add     bool `docopt:"add"`
	Delete  bool `docopt:"delete"`

	// Options for list
	FormatTemplate string `docopt:"-f,--format"`
	Domain         string `docopt:"-d,--domain"`
	SearchQuery    string `docopt:"-s,--search"`
	Tag            string `docopt:"-t,--tag"`
	Sort           string `docopt:"-o,--sort"`
	Cull           bool   `docopt:"--cull"`
	DeleteAll      bool   `docopt:"--delete"`

	// Parameter for archive and delete
	ItemID int `docopt:"<item-id>"`

	// Options for add
	URL   string `docopt:"<url>"`
	Title string `docopt:"--title"`
	Tags  string `docopt:"--tags"`
}

func main() {
	usage := `A Pocket <getpocket.com> client.

Usage:
  pocket list [--format=<template>] [--domain=<domain>] [--tag=<tag>] [--search=<query>] [--sort=<sort>] [--cull|--delete]
  pocket archive <item-id>
  pocket delete <item-id>
  pocket add <url> [--title=<title>] [--tags=<tags>]

Options for list:
  -f, --format <template> A Go template to show items.
  -d, --domain <domain>   Filter items by its domain when listing.
  -s, --search <query>    Search query when listing.
  -t, --tag <tag>         Filter items by a tag when listing.
  -o, --sort <sort>       Sort items by "newest", "oldest", "title", or "site"
  --cull                  Open items one by one in a browser and prompt to delete each one
  --delete                Delete all items retrieved

Options for add:
  --title <title>         A manually specified title for the article
  --tags <tags>           A comma-separated list of tags
`
	opts, err := docopt.ParseArgs(usage, nil, version)
	if err != nil {
		panic(err)
	}

	var conf Config
	err = opts.Bind(&conf)
	if err != nil {
		panic(err)
	}

	consumerKey := getConsumerKey()

	accessToken, err := restoreAccessToken(consumerKey)
	if err != nil {
		panic(err)
	}

	client := api.NewClient(consumerKey, accessToken.AccessToken)

	switch {
	case conf.List:
		commandList(conf, client)
	case conf.Archive:
		commandArchive(conf, client)
	case conf.Delete:
		commandDelete(conf, client)
	case conf.Add:
		commandAdd(conf, client)
	default:
		panic("Not implemented")
	}
}

type bySortID []api.Item

func (s bySortID) Len() int           { return len(s) }
func (s bySortID) Less(i, j int) bool { return s[i].SortId < s[j].SortId }
func (s bySortID) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// confirm asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. If the input is not recognized, it will ask again. The function does not return
// until it gets a valid response from the user.
func confirm(s string) bool {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf("%s [y/n]: ", s)

		response, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		response = strings.ToLower(strings.TrimSpace(response))

		switch response {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		case "q", "quit":
			fmt.Fprintln(os.Stderr, "Quitting. Bye!")
			os.Exit(1)
		}
	}
}

func commandList(conf Config, client *api.Client) {
	options := api.RetrieveOption{
		Domain: conf.Domain,
		Search: conf.SearchQuery,
		Tag:    conf.Tag,
		Sort:   api.Sort(conf.Sort),
	}

	res, err := client.Retrieve(&options)
	if err != nil {
		panic(err)
	}

	var itemTemplate *template.Template
	if conf.FormatTemplate != "" {
		itemTemplate = template.Must(template.New("item").Parse(conf.FormatTemplate))
	} else {
		itemTemplate = defaultItemTemplate
	}

	items := []api.Item{}
	for _, item := range res.List {
		items = append(items, item)
	}
	if conf.DeleteAll {
		if confirm(fmt.Sprintf("Really delete %d items?", len(items))) {
			deleteItems := []*api.Action{}
			for _, item := range items {
				deleteItems = append(deleteItems, api.NewDeleteAction(item.ItemID))
			}
			res, err := client.Modify(deleteItems...)
			if err != nil {
				fmt.Printf("%#v, %v\n", res, err)
			}
		}
		return
	}
	sort.Sort(bySortID(items))
	seenURLs := map[string]struct{}{}
	itemsLen := len(items)
	for i, item := range items {
		fmt.Printf("%d/%d ", i+1, itemsLen)
		err := itemTemplate.Execute(os.Stdout, item)
		if err != nil {
			panic(err)
		}
		url := CleanURL(item.URL())
		if _, found := seenURLs[url]; found {
			fmt.Println("\nItem already seen. Deleting...")
			action := api.NewDeleteAction(item.ItemID)
			res, err := client.Modify(action)
			if err != nil {
				fmt.Printf("%#v, %v\n", res, err)
			}
			fmt.Println("")
			continue
		} else {
			seenURLs[url] = struct{}{}
		}
		if conf.Cull {
			chk, err := http.Head(item.URL())
			if err != nil {
				fmt.Printf("\nGot an err when HEADing: %s, GETting instead...\n", err.Error())
				chk, err = http.Get(item.URL())
				if err != nil {
					fmt.Printf("\n%s\n", err.Error())
				}
			}
			if err != nil && !strings.HasSuffix(err.Error(), ": EOF") {
				if body, err := io.ReadAll(chk.Body); err != nil &&
					(strings.Contains(string(body), "isn't available anymore") ||
						strings.Contains(string(body), "this page doesn")) {
					chk.StatusCode = http.StatusNotFound
					chk.Status = "Not Available"
				}
			}
			if err == nil && chk.StatusCode <= http.StatusPermanentRedirect {
				fmt.Printf(" %s\n", chk.Status)
				fin := chk.Request.URL.String()
				openPrompt := "Open?"
				if fin != item.URL() {
					openPrompt = fmt.Sprintf("Open %s?", fin)
				}
				if confirm(openPrompt) {
					cmd := exec.Command("firefox", "--new-tab", fin)
					if _, err := cmd.Output(); err != nil {
						if exitErr, ok := err.(*exec.ExitError); ok {
							log.Fatalf("Failed to run firefox: %s, %s", err, exitErr.Stderr)
						}
						log.Fatalf("Failed to run firefox: %s", err)
					}
				}
			} else if (err != nil && !strings.HasSuffix(err.Error(), ": EOF")) || err == nil {
				fmt.Printf("\nStatus was %s\n", chk.Status)
			}
			if confirm("Delete?") {
				action := api.NewDeleteAction(item.ItemID)
				res, err := client.Modify(action)
				if err != nil {
					fmt.Printf("%#v, %v\n", res, err)
				}
			}
		}
		fmt.Println("")
	}
}

func commandArchive(conf Config, client *api.Client) {
	if conf.ItemID != 0 {
		action := api.NewArchiveAction(conf.ItemID)
		res, err := client.Modify(action)
		fmt.Println(res, err)
	} else {
		panic("Wrong arguments, need <item-id>")
	}
}

func commandDelete(conf Config, client *api.Client) {
	if conf.ItemID != 0 {
		action := api.NewDeleteAction(conf.ItemID)
		res, err := client.Modify(action)
		if err != nil {
			fmt.Println(res, err)
		} else {
			fmt.Printf("Deleted item %d\n", conf.ItemID)
		}
	} else {
		panic("Wrong arguments, need <item-id>")
	}
}

func commandAdd(conf Config, client *api.Client) {
	if conf.URL == "" {
		panic("Wrong arguments, need <url>")
	}

	options := api.AddOption{
		URL:   conf.URL,
		Title: conf.Title,
		Tags:  conf.Tags,
	}

	err := client.Add(&options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func getConsumerKey() string {
	consumerKeyPath := filepath.Join(configDir, "consumer_key")
	consumerKey, err := ioutil.ReadFile(consumerKeyPath)

	if err != nil {
		log.Printf("Can't get consumer key: %v\n", err)
		log.Print("Enter your consumer key (from here https://getpocket.com/developer/apps/): ")

		consumerKey, _, err = bufio.NewReader(os.Stdin).ReadLine()
		if err != nil {
			panic(err)
		}

		err = ioutil.WriteFile(consumerKeyPath, consumerKey, 0600)
		if err != nil {
			panic(err)
		}

		return string(consumerKey)
	}

	return string(bytes.SplitN(consumerKey, []byte("\n"), 2)[0])
}

func restoreAccessToken(consumerKey string) (*auth.Authorization, error) {
	accessToken := &auth.Authorization{}
	authFile := filepath.Join(configDir, "auth.json")

	err := loadJSONFromFile(authFile, accessToken)

	if err != nil {
		log.Println(err)

		accessToken, err = obtainAccessToken(consumerKey)
		if err != nil {
			return nil, err
		}

		err = saveJSONToFile(authFile, accessToken)
		if err != nil {
			return nil, err
		}
	}

	return accessToken, nil
}

func obtainAccessToken(consumerKey string) (*auth.Authorization, error) {
	ch := make(chan struct{})
	ts := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/favicon.ico" {
				http.Error(w, "Not Found", 404)
				return
			}

			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintln(w, "Authorized.")
			ch <- struct{}{}
		}))
	defer ts.Close()

	redirectURL := ts.URL

	requestToken, err := auth.ObtainRequestToken(consumerKey, redirectURL)
	if err != nil {
		return nil, err
	}

	url := auth.GenerateAuthorizationURL(requestToken, redirectURL)
	fmt.Println(url)

	<-ch

	return auth.ObtainAccessToken(consumerKey, requestToken)
}

func saveJSONToFile(path string, v interface{}) error {
	w, err := os.Create(path)
	if err != nil {
		return err
	}

	defer w.Close()

	return json.NewEncoder(w).Encode(v)
}

func loadJSONFromFile(path string, v interface{}) error {
	r, err := os.Open(path)
	if err != nil {
		return err
	}

	defer r.Close()

	return json.NewDecoder(r).Decode(v)
}
