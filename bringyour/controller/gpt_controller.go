package controller

import (
    _ "embed"
    "context"
	"fmt"
	"strings"
    "time"
    "os"
    "os/exec"
    "path/filepath"
    "sync"
    "errors"
    "regexp"
    mathrand "math/rand"
    "slices"
    "net/url"
    "net/mail"

    "golang.org/x/net/html"
    "golang.org/x/exp/maps"

    "github.com/microcosm-cc/bluemonday"

	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
)


//go:embed by-privacy.txt
var byPrivacyPolicyText string
const byNoticeEmail = "notice@bringyour.com"

const maxPrivacyPolicyAge = 24 * time.Hour

const collectTimeout = 30 * time.Second
const linkTimeout = 10 * time.Second


func genericPrivacyPolicyText(serviceName string) string {
    privacyPolicyText := byPrivacyPolicyText
    privacyPolicyText = strings.ReplaceAll(privacyPolicyText, "Bring Your, LLC", privacyPolicyText)
    privacyPolicyText = strings.ReplaceAll(privacyPolicyText, "BringYour", privacyPolicyText)

    return privacyPolicyText
}


type GptPrivacyPolicyArgs struct {
	ServiceName string `json:"service_name"`
	ServiceUrls []string `json:"service_urls"`
}

type GptPrivacyPolicyResult struct {
	Found bool `json:"found"`
	PrivacyPolicy string `json:"privacy_policy",omitempty`
}

func GptPrivacyPolicy(
	privacyPolicy *GptPrivacyPolicyArgs,
	clientSession *session.ClientSession,
) (*GptPrivacyPolicyResult, error) {
	fmt.Printf("GptPrivacyPolicy %v\n", privacyPolicy)

    completePrivacyPolicy, err := model.GetCompletePrivacyPolicy(
        clientSession.Ctx,
        privacyPolicy.ServiceName,
    )
    var privacyPolicyText string
    if err == nil {
        privacyPolicyText = completePrivacyPolicy.PrivacyPolicyText

        // check soft refresh
        age := time.Now().Sub(completePrivacyPolicy.CreateTime)
        
        var refresh bool
        if maxPrivacyPolicyAge <= age {
            refresh = true
        } else {
            // refresh probability 1/(remaining minutes)
            refresh = (mathrand.Int63n(int64((maxPrivacyPolicyAge - age) / time.Minute)) == 0)
        }

        if refresh {
            // attempt to refresh in the background
            go func() {
                collector := NewPrivacyPolicyCollector(
                    clientSession.Ctx,
                    privacyPolicy.ServiceName,
                    privacyPolicy.ServiceUrls,
                    collectTimeout,
                    linkTimeout,
                )
                privacyPolicyText, extractedUrls, err := collector.Run()
                if err == nil {
                    model.SetCompletePrivacyPolicy(
                        clientSession.Ctx,
                        model.NewCompletePrivacyPolicy(
                            privacyPolicy.ServiceName,
                            privacyPolicy.ServiceUrls,
                            privacyPolicyText,
                            extractedUrls,
                        ),
                    )
                }
            }()
        }
    } else {
        collector := NewPrivacyPolicyCollector(
            clientSession.Ctx,
            privacyPolicy.ServiceName,
            privacyPolicy.ServiceUrls,
            collectTimeout,
            linkTimeout,
        )
        privacyPolicyText, extractedUrls, err := collector.Run()

        if err == nil {
            model.SetCompletePrivacyPolicy(
                clientSession.Ctx,
                model.NewCompletePrivacyPolicy(
                    privacyPolicy.ServiceName,
                    privacyPolicy.ServiceUrls,
                    privacyPolicyText,
                    extractedUrls,
                ),
            )
        } else {
            model.SetCompletePrivacyPolicy(
                clientSession.Ctx,
                model.NewCompletePrivacyPolicyPending(
                    privacyPolicy.ServiceName,
                    privacyPolicy.ServiceUrls,
                ),
            )
            // use a stand in policy to advance the gpt
            // this will be cleaned up async in the future after manual inspection
            privacyPolicyText = genericPrivacyPolicyText(privacyPolicy.ServiceName)
        }
    }

    // add a best effort contact email address
    // use `byNoticeEmail` to advance the gpt conversation and flag the request for later review
    if !hasEmailAddress(privacyPolicyText) {
        privacyPolicyText = fmt.Sprintf(`

BringYour Privacy Agent believes the best email address to reach %s is "%s". Please use this email address in correspondence.


%s`,
            privacyPolicy.ServiceName,
            // TODO have a model to support custom overrides per service
            // TODO use the by notice for now
            byNoticeEmail,
            privacyPolicyText,
        )
    }

	return &GptPrivacyPolicyResult{
		Found: true,
		PrivacyPolicy: privacyPolicyText,
	}, nil
}


func hasEmailAddress(privacyPolicyText string) bool {
    maybeEmailPattern := regexp.MustCompile("[\\w-\\.+]+@([\\w-]+\\.)+[\\w-]{2,64}")
    allGroups := maybeEmailPattern.FindAllStringSubmatch(privacyPolicyText, -1)
    for _, groups := range allGroups {
        maybeEmail := groups[0]
        if _, err := mail.ParseAddress(maybeEmail); err == nil {
            fmt.Printf("FOUND EMAIL ADDRESS: %s\n", maybeEmail)
            return true
        }
    }
    return false
}


func GptBeMyPrivacyAgent(
	beMyPrivacyAgent *model.GptBeMyPrivacyAgentArgs,
	clientSession *session.ClientSession,
) (*model.GptBeMyPrivacyAgentResult, error) {
	fmt.Printf("GptBeMyPrivacyAgent %v\n", beMyPrivacyAgent)

    // FIXME if to is genericPolicyEmail, then just save the request. We need to find the policy and try again.
	
	return model.SetGptBeMyPrivacyAgentPending(beMyPrivacyAgent, clientSession)
}



type PrivacyPolicyCollector struct {
    ctx context.Context
    serviceName string
    serviceUrls []string
    collectTimeout time.Duration
    linkTimeout time.Duration
}

func NewPrivacyPolicyCollector(
    ctx context.Context,
    serviceName string,
    serviceUrls []string,
    collectTimeout time.Duration,
    linkTimeout time.Duration,
) *PrivacyPolicyCollector {
    return &PrivacyPolicyCollector{
        ctx: ctx,
        serviceName: serviceName,
        serviceUrls: serviceUrls,
        collectTimeout: collectTimeout,
        linkTimeout: linkTimeout,
    }
}


// load the first page
// find all links with the word "privacy"
// if more than one, use all of them
// scrape all text from the privacy pages
// explore one link deep
// keep a list of text nodes per page where that text has not appeared in another sequence
// keep a map of text to count for all pages
// only process pages where <html lang="en">
func (self *PrivacyPolicyCollector) Run() (string, []string, error) {
    privacyLinkTextPattern := regexp.MustCompile("(?i)\\bprivacy\\b")
    for _, serviceUrl := range self.serviceUrls {
        // TODO check privacy.txt for the domain

        if bodyHtml, err := GetBodyHtml(self.ctx, serviceUrl, self.linkTimeout); err == nil {
            links, err := findLinks(serviceUrl, bodyHtml, func(a *html.Node, linkText string, linkUrl string)(bool) {
                return privacyLinkTextPattern.MatchString(linkText)
            })

            if err == nil && 0 < len(links) {
                extractor := NewTextExtractor(self.ctx, self.linkTimeout)

                for _, link := range links {
                    extractor.Add(link, 1)
                }
                extractor.SetEndOnQuiet()

                select {
                case <- self.ctx.Done():
                case <- extractor.End:
                case <- time.After(self.collectTimeout):
                }

                extractedText, extractedLinks := extractor.Finish()
                extractedUrls := []string{}
                for _, extractedLink := range extractedLinks {
                    extractedUrls = append(extractedUrls, extractedLink.Url)
                }

                if 0 < len(extractedUrls) {
                    return extractedText, extractedUrls, nil
                }
            }
        }

    }
    return "", nil, errors.New("Could not extract any text.")
}


type TextExtractor struct {
    ctx context.Context
    cancel context.CancelFunc

    linkTimeout time.Duration

    extractedStateLock sync.Mutex
    addedLinkUrls map[string]int
    extractedText map[*Link]string

    inFlightCount int
    endOnQuiet bool

    End chan struct{}
}

func NewTextExtractor(ctx context.Context, linkTimeout time.Duration) *TextExtractor {

    cancelCtx, cancel := context.WithCancel(ctx)


    return &TextExtractor{
        ctx: cancelCtx,
        cancel: cancel,
        linkTimeout: linkTimeout,
        addedLinkUrls: map[string]int{},
        extractedText: map[*Link]string{},
        inFlightCount: 0,
        endOnQuiet: false,
        End: make(chan struct{}),
    }
}

func (self *TextExtractor) Add(link *Link, depthToLive int) {
    fmt.Printf("EXTRACTOR ADD LINK: (%s) %s\n", link.Text, link.Url)

    select {
    case <- self.ctx.Done():
        return
    default:
    }

    self.extractedStateLock.Lock()
    _, alreadyAdded := self.addedLinkUrls[link.Url]
    if !alreadyAdded {
        self.addedLinkUrls[link.Url] = depthToLive
        self.inFlightCount += 1
    }
    self.extractedStateLock.Unlock()

    if alreadyAdded {
        return
    }

    go func() {
        if bodyHtml, err := GetBodyHtml(self.ctx, link.Url, self.linkTimeout); err == nil {
            if bodyText, err := findText(bodyHtml, basicTextCallback); err == nil {
                self.extractedStateLock.Lock()
                select {
                case <- self.ctx.Done():
                default:
                    self.extractedText[link] = bodyText
                }
                self.extractedStateLock.Unlock()

                if 0 < depthToLive {
                    privacyLinkTextPattern := regexp.MustCompile("(?i)\\bprivacy\\b")

                    links, err := findLinks(link.Url, bodyHtml, func(a *html.Node, linkText string, linkUrl string)(bool) {
                        return privacyLinkTextPattern.MatchString(linkText) || privacyLinkTextPattern.MatchString(linkUrl)
                    })

                    if err == nil {
                        for _, link := range links {
                            self.Add(link, depthToLive - 1)
                        }
                    }
                }


                self.extractedStateLock.Lock()
                self.inFlightCount -= 1
                end := (self.inFlightCount == 0) && self.endOnQuiet
                self.extractedStateLock.Unlock()

                if end {
                    close(self.End)
                }
            }
        }
    }()
}

func (self *TextExtractor) SetEndOnQuiet() {
    self.extractedStateLock.Lock()
    defer self.extractedStateLock.Unlock()
    self.endOnQuiet = true
}

func (self *TextExtractor) Finish() (string, []*Link) {
    self.cancel()

    self.extractedStateLock.Lock()
    defer self.extractedStateLock.Unlock()

    links := maps.Keys(self.extractedText)

    privacyLinkTextPattern := regexp.MustCompile("(?i)\\bprivacy\\b")
    slices.SortFunc(links, func(a *Link, b *Link)(int) {
        aPrivacy := privacyLinkTextPattern.MatchString(a.Text)
        bPrivacy := privacyLinkTextPattern.MatchString(b.Text)
        if aPrivacy != bPrivacy {
            // privacy first
            if aPrivacy {
                return -1
            } else {
                return 1
            }
        }

        aDepth := self.addedLinkUrls[a.Url]
        bDepth := self.addedLinkUrls[b.Url]
        if aDepth != bDepth {
            // higher depth first
            if bDepth < aDepth {
                return -1
            } else {
                return 1
            }
        }

        return strings.Compare(a.Text, b.Text)
    })


    outDoubleLines := [][]string{}

    usedLines := map[string]bool{}
    for _, link := range links {
        doubleLines := strings.Split(self.extractedText[link], "\n\n")
        for _, doubleLine := range doubleLines {
            lines := strings.Split(doubleLine, "\n")

            outLines := []string{}

            for _, line := range lines {
                if !usedLines[line] {
                    usedLines[line] = true
                    outLines = append(outLines, line)
                }
            }

            if 0 < len(outLines) {
                outDoubleLines = append(outDoubleLines, outLines)
            }
        }
    }

    outDouble := []string{}
    for _, outLines := range outDoubleLines {
        outDouble = append(outDouble, strings.Join(outLines, "\n"))
    }

    out := strings.Join(outDouble, "\n\n")

    return out, links 
}


type Link struct {
    Url string
    Text string
}


type linkCallback func(a *html.Node, linkText string, linkUrl string)(bool)


func allLinksCallback(a *html.Node, linkText string, linkUrl string) bool {
    return true
}


func findLinks(bodyUrl string, bodyHtml string, callback linkCallback) ([]*Link, error) {
    doc, err := html.Parse(strings.NewReader(bodyHtml))
    if err != nil {
        return nil, err
    }

    links := []*Link{}

    var dfs func(*html.Node)
    dfs = func(n *html.Node) {
        if n.Type == html.ElementNode && n.Data == "a" {
            if linkText, err := findTextInElement(n, basicTextCallback); err == nil {
                var url string
                for _, a := range n.Attr {
                    if a.Key == "href" {
                        url = a.Val
                        break
                    }
                }

                if url != "" {
                    if absUrl := absUrl(bodyUrl, url); absUrl != "" {
                        if callback(n, linkText, absUrl) {
                            link := &Link{
                                Url: absUrl,
                                Text: linkText,
                            }
                            links = append(links, link)
                        }
                    }
                }
            }
        } else {
            for c := n.FirstChild; c != nil; c = c.NextSibling {
                dfs(c)
            }
        }
    }
    dfs(doc)

    return links, nil
}


func absUrl(absSourceUrl string, hrefUrl string) string {
    fixAbsUrl := func(u *url.URL) string {
        if u.Scheme == "http" {
            u.Scheme = "https"
        }
        // for our use case the fragment is useless
        u.Fragment = ""
        u.RawFragment = ""
        return u.String()
    }

    href, err := url.Parse(hrefUrl)
    if err != nil {
        return ""
    }

    if href.IsAbs() {
        return fixAbsUrl(href)
    }

    source, err := url.Parse(absSourceUrl)
    if err != nil {
        return ""
    }

    absHref := source.ResolveReference(href)
    return fixAbsUrl(absHref)
}


type textCallback func(n *html.Node)(before string, after string, descend bool)


func basicTextCallback(n *html.Node) (before string, after string, descend bool) {
    if n.Type == html.TextNode {
        before = strings.TrimSpace(n.Data)
    } else if n.Type == html.ElementNode {
        descend = true
        switch n.Data {
        case "h", "h1", "h2", "h3", "h4", "p", "br", "hr", "li":
            before = "\n\n"
            after = "\n\n"
        case "script", "style":
            descend = false
        default:
            before = " "
            after = " "
        }
    } else if n.Type == html.DocumentNode {
        descend = true
    }

    return
}


func findText(bodyHtml string, callback textCallback) (string, error) {
    doc, err := html.Parse(strings.NewReader(bodyHtml))
    if err != nil {
        return "", err
    }

    if bodyText, err := findTextInElement(doc, callback); err == nil {
        return bodyText, nil
    }

    // the html is malformed
    // fall back to try blue monday heuristic sanitizer
    bodyText := bluemonday.UGCPolicy().Sanitize(bodyHtml)
    return bodyText, nil
}


func findTextInElement(top *html.Node, callback textCallback) (string, error) {
    parts := []string{}

    var dfs func(*html.Node)
    dfs = func(n *html.Node) {
        before, after, descend := callback(n)

        if before != "" {
            parts = append(parts, before)
        }
        if descend {
            for c := n.FirstChild; c != nil; c = c.NextSibling {
                dfs(c)
            }
        }
        if after != "" {
            parts = append(parts, after)
        }
    }
    dfs(top)

    text := strings.Join(parts, "")
    // text = strings.TrimSpace(text)

    // combine multiple whitespace into a single whitespace
    // - if spaces contain a newline, keep the newline
    // - else use a space
    parts = []string{}
    spacePattern := regexp.MustCompile("\\s+")
    allIndexes := spacePattern.FindAllStringSubmatchIndex(text, -1)
    i := 0
    for _, indexes := range allIndexes {
        parts = append(parts, text[i:indexes[0]])
        newlineCount := strings.Count(text[indexes[0]:indexes[1]], "\n")
        if 2 <= newlineCount {
            parts = append(parts, "\n\n")
        } else if newlineCount == 1 {
            parts = append(parts, "\n")
        } else {
            parts = append(parts, " ")
        }
        i = indexes[1]
    }
    parts = append(parts, text[i:len(text)])

    text = strings.Join(parts, "")
    text = strings.TrimSpace(text)

    return text, nil
}





// scrapes a url for the rendered body html
// this uses a headless browser scaper called `innerhtml`
func GetBodyHtml(ctx context.Context, url string, timeout time.Duration) (string, error) {
    binPath, err := os.Executable()
    if err != nil {
        return "", err
    }
    // the `innerhtml` scraping module should be in the same dir as the binary
    // it is versioned at `gpt/innerhtml`
    innerHtmlPath := fmt.Sprintf("%s/innerhtml/index.js", filepath.Dir(binPath))

    c := exec.CommandContext(
        ctx,
        "node",
        innerHtmlPath,
        url,
        fmt.Sprintf("%d", timeout / time.Millisecond),
    )
    bodyHtmlBytes, err := c.Output()
    if err != nil {
        return "", err
    }
    return string(bodyHtmlBytes), nil
}

