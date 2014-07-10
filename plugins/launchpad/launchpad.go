package launchpad

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"gopkg.in/niemeyer/mup.v0"
	"gopkg.in/tomb.v2"
	"io/ioutil"
	"labix.org/v2/mgo/bson"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func init() {
	mup.RegisterPlugin("lpshowbugs", startPlugin)
	mup.RegisterPlugin("lptrackbugs", startPlugin)
	mup.RegisterPlugin("lptrackmerges", startPlugin)
}

var httpClient = http.Client{Timeout: mup.NetworkTimeout}

type lpPluginMode int

const (
	showBugsMode lpPluginMode = iota + 1
	trackBugsMode
	trackMergesMode
)

var pluginModes = map[string]lpPluginMode{
	"lpshowbugs":    showBugsMode,
	"lptrackbugs":   trackBugsMode,
	"lptrackmerges": trackMergesMode,
}

type lpPlugin struct {
	mode lpPluginMode

	mu       sync.Mutex
	tomb     tomb.Tomb
	plugger  *mup.Plugger
	messages chan *lpMessage
	settings struct {
		OAuthAccessToken string
		OAuthSecretToken string

		BaseURL   string
		Project   string
		Options   string
		PrefixNew string
		PrefixOld string

		HandleTimeout bson.Duration
		PollDelay     bson.Duration
	}
}

const (
	defaultHandleTimeout    = 500 * time.Millisecond
	defaultBaseURL          = "https://api.launchpad.net/1.0/"
	defaultBaseURLTrackBugs = "https://launchpad.net/"
	defaultPollDelay        = 10 * time.Second
	defaultPrefix           = "Bug #%d changed"
)

func startPlugin(plugger *mup.Plugger) mup.Plugin {
	mode := pluginModes[strings.SplitN(plugger.Name(), ":", 2)[0]]
	if mode == 0 {
		panic("launchpad plugin used under unknown name: " + plugger.Name())
	}
	p := &lpPlugin{
		mode:     mode,
		plugger:  plugger,
		messages: make(chan *lpMessage),
	}
	plugger.Settings(&p.settings)
	if p.settings.HandleTimeout.Duration == 0 {
		p.settings.HandleTimeout.Duration = defaultHandleTimeout
	}
	if p.settings.PollDelay.Duration == 0 {
		p.settings.PollDelay.Duration = defaultPollDelay
	}
	if p.settings.BaseURL == "" {
		if mode == trackBugsMode {
			p.settings.BaseURL = defaultBaseURLTrackBugs
		} else {
			p.settings.BaseURL = defaultBaseURL
		}
	}
	if p.settings.PrefixNew == "" {
		p.settings.PrefixNew = defaultPrefix
	}
	if p.settings.PrefixOld == "" {
		p.settings.PrefixOld = defaultPrefix
	}
	switch p.mode {
	case showBugsMode:
		p.tomb.Go(p.loop)
	case trackBugsMode:
		p.tomb.Go(p.pollBugs)
	case trackMergesMode:
		p.tomb.Go(p.pollMerges)
	default:
		panic("internal error: unknown launchpad plugin mode")
	}
	return p
}

func (p *lpPlugin) Stop() error {
	p.tomb.Kill(nil)
	return p.tomb.Wait()
}

type lpMessage struct {
	msg  *mup.Message
	bugs []int
}

func (p *lpPlugin) Handle(msg *mup.Message) error {
	if p.mode != showBugsMode {
		return nil
	}
	bmsg := &lpMessage{msg, parseBugs(msg.Text)}
	if len(bmsg.bugs) == 0 {
		return nil
	}
	select {
	case p.messages <- bmsg:
	case <-time.After(p.settings.HandleTimeout.Duration):
		p.plugger.Replyf(msg, "The Launchpad server seems a bit sluggish right now. Please try again soon.")
	}
	return nil
}

func (p *lpPlugin) loop() error {
	for {
		select {
		case bmsg := <-p.messages:
			err := p.handle(bmsg)
			if err != nil {
				p.plugger.Logf("Error talking to Launchpad: %v")
			}
		case <-p.tomb.Dying():
			return nil
		}
	}
	return nil
}

func (p *lpPlugin) handle(bmsg *lpMessage) error {
	for _, id := range bmsg.bugs {
		_ = p.showBug(bmsg.msg.Account, bmsg.msg.ReplyTarget(), id, "")
	}
	return nil
}

type lpBug struct {
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	TasksLink string   `json:"bug_tasks_collection_link"`
}

type lpBugTasks struct {
	Entries []lpBugEntry `json:"entries"`
}

type lpBugEntry struct {
	Target       string `json:"bug_target_display_name"`
	Status       string `json:"status"`
	AssigneeLink string `json:"assignee_link"`
}

func (p *lpPlugin) showBug(account, target string, bugId int, prefix string) error {
	var bug lpBug
	var tasks lpBugTasks
	err := p.request("/bugs/"+strconv.Itoa(bugId), &bug)
	if err != nil {
		return err
	}
	if bug.TasksLink != "" {
		err = p.request(bug.TasksLink, &tasks)
		if err != nil {
			return err
		}
	}
	if !strings.Contains(prefix, "%d") || strings.Count(prefix, "%") > 1 {
		prefix = "Bug #%d"
	}
	return p.plugger.Sendf(account, target, prefix+": %s%s <https://launchpad.net/bugs/%d>", bugId, bug.Title, p.formatNotes(&bug, &tasks), bugId)
}

func (p *lpPlugin) formatNotes(bug *lpBug, tasks *lpBugTasks) string {
	var buf bytes.Buffer
	buf.Grow(256)
	for _, tag := range bug.Tags {
		buf.WriteString(" <")
		buf.WriteString(tag)
		buf.WriteString(">")
	}
	for _, entry := range tasks.Entries {
		buf.WriteString(" <")
		buf.WriteString(entry.Target)
		buf.WriteString(":")
		buf.WriteString(entry.Status)
		if i := strings.Index(entry.AssigneeLink, "~"); i > 0 {
			if entry.Status == "New" || entry.Status == "Confirmed" {
				buf.WriteString(" for ")
			} else {
				buf.WriteString(" by ")
			}
			buf.WriteString(entry.AssigneeLink[i+1:])
		}
		buf.WriteString(">")
	}
	return buf.String()
}

func (p *lpPlugin) request(url string, result interface{}) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = p.settings.BaseURL + url
	}
	if p.settings.Options != "" {
		if strings.Contains(url, "?") {
			url += "&" + p.settings.Options
		} else {
			url += "?" + p.settings.Options
		}
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		p.plugger.Logf("Cannot perform Launchpad request: %v", err)
		return fmt.Errorf("cannot perform Launchpad request: %v", err)
	}
	defer resp.Body.Close()
	if strings.Contains(url, "/+bugs-text") {
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			p.plugger.Logf("Cannot read Launchpad response: %v", err)
			return fmt.Errorf("cannot read Launchpad response: %v", err)
		}
		*(result.(*[]int)) = parseShowBugs(string(data))
		return nil
	}
	err = json.NewDecoder(resp.Body).Decode(result)
	if err != nil {
		p.plugger.Logf("Cannot decode Launchpad response: %v", err)
		return fmt.Errorf("cannot decode Launchpad response: %v", err)
	}
	return nil
}

var bugre = regexp.MustCompile(`(?i)(?:bugs?[ /]#?([0-9]+)|(?:^|\W)#([0-9]{5,}))`)

func parseBugs(text string) []int {
	var bugs []int
	for _, match := range bugre.FindAllStringSubmatch(text, -1) {
		s := match[1]
		if s == "" {
			s = match[2]
		}
		id, err := strconv.Atoi(s)
		if err != nil {
			panic("bug id not an int, which must never happen (regexp is broken)")
		}
		bugs = append(bugs, id)
	}
	return bugs
}

func parseShowBugs(data string) []int {
	var bugs []int
	for _, s := range strings.Fields(data) {
		id, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		bugs = append(bugs, id)
	}
	sort.Ints(bugs)
	return bugs
}

func (p *lpPlugin) pollBugs() error {
	var oldBugs []int
	var first = true
	for {
		select {
		case <-time.After(p.settings.PollDelay.Duration):
		case <-p.tomb.Dying():
			return nil
		}

		var newBugs []int
		err := p.request("/"+p.settings.Project+"/+bugs-text", &newBugs)
		if err != nil {
			continue
		}

		if first {
			first = false
			oldBugs = newBugs
			continue
		}

		var o, n int
		for o < len(oldBugs) || n < len(newBugs) {
			var prefix string
			var bugId int
			switch {
			case o == len(oldBugs) || n < len(newBugs) && newBugs[n] < oldBugs[o]:
				prefix = p.settings.PrefixNew
				bugId = newBugs[n]
				n++
			case n == len(newBugs) || o < len(oldBugs) && oldBugs[o] < newBugs[n]:
				prefix = p.settings.PrefixOld
				bugId = oldBugs[o]
				o++
			default:
				o++
				n++
				continue
			}

			// TODO Support plugin targets.
			p.showBug("canonical", "#mup-test", bugId, prefix)
		}

		oldBugs = newBugs
	}
	return nil
}

type lpMerges struct {
	Entries []lpMergeEntry
}

type lpMergeEntry struct {
	SelfLink    string `json:"self_link"`
	Status      string `json:"queue_status"`
	Description string `json:"description"`
}

func (e *lpMergeEntry) Id() (id int, ok bool) {
	i := strings.LastIndex(e.SelfLink, "/")
	if i < 0 {
		return 0, false
	}
	id, err := strconv.Atoi(e.SelfLink[i+1:])
	if err != nil {
		return 0, false
	}
	return id, true
}

func (e *lpMergeEntry) URL() (url string, ok bool) {
	i := strings.Index(e.SelfLink, "~")
	if i < 0 {
		return "", false
	}
	return "https://launchpad.net/" + e.SelfLink[i:], true
}

func (p *lpPlugin) pollMerges() error {
	oldMerges := make(map[int]string)
	first := true
	for {
		select {
		case <-time.After(p.settings.PollDelay.Duration):
		case <-p.tomb.Dying():
			return nil
		}

		var newMerges lpMerges
		err := p.request("/"+p.settings.Project+"?ws.op=getMergeProposals", &newMerges)
		if err != nil {
			continue
		}

		for _, merge := range newMerges.Entries {
			id, ok := merge.Id()
			if !ok || oldMerges[id] == merge.Status {
				continue
			}
			oldMerges[id] = merge.Status
			url, ok := merge.URL()
			if !ok || first {
				continue
			}

			// TODO Support plugin targets.
			p.plugger.Sendf("canonical", "#mup-test", "Merge proposal changed [%s]: %s <%s>", strings.ToLower(merge.Status), firstSentence(merge.Description), url)
		}
		first = false
	}
	return nil
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	if len(s) > 80 {
		if i := strings.LastIndex(s[:80], " "); i > 0 {
			return s[:i] + " (...)"
		}
		return s[:80] + "(...)"
	}
	return s
}
