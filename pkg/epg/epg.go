package epg

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/valyala/fasthttp"

	"github.com/jiotv-go/jiotv_go/v3/pkg/scheduler"
	"github.com/jiotv-go/jiotv_go/v3/pkg/utils"
)

const (
	// URL for fetching channels from JioTV API
	CHANNEL_URL = "https://jiotv.data.cdn.jio.com/apis/v3.0/getMobileChannelList/get/?os=android&devicetype=phone&usertype=tvYR7NSNn7rymo3F"
	// URL for fetching EPG data for individual channels from JioTV API
	EPG_URL = "https://jiotv.data.cdn.jio.com/apis/v1.3/getepg/get/?offset=%d&channel_id=%d"
	// EPG_POSTER_URL
	EPG_POSTER_URL = "https://jiotv.catchup.cdn.jio.com/dare_images/shows"
	// EPG_TASK_ID is the ID of the EPG generation task
	EPG_TASK_ID = "jiotv_epg"
)

// Init initializes EPG generation and schedules it for the next day.
func Init() {
	epgFile := "epg.xml.gz"
	var lastModTime time.Time
	flag := false
	utils.Log.Println("Checking EPG file")
	if stat, err := os.Stat(epgFile); err == nil {
		// If file was modified today, don't generate new EPG
		// Else generate new EPG
		lastModTime = stat.ModTime()
		fileDate := lastModTime.Format("2006-01-02")
		todayDate := time.Now().Format("2006-01-02")
		if fileDate == todayDate {
			utils.Log.Println("EPG file is up to date.")
		} else {
			utils.Log.Println("EPG file is old.")
			flag = true
		}
	} else {
		utils.Log.Println("EPG file doesn't exist")
		flag = true
	}

	genepg := func() error {
		fmt.Println("\tGenerating new EPG file... Please wait.")
		err := GenXMLGz(epgFile)
		if err != nil {
			utils.Log.Fatal(err)
		}
		return err
	}

	if flag {
		genepg()
	}
	// setup random time to avoid server load
	random_hour_bigint, err := rand.Int(rand.Reader, big.NewInt(3))
	if err != nil {
		panic(err)
	}
	random_min_bigint, err := rand.Int(rand.Reader, big.NewInt(60))
	if err != nil {
		panic(err)
	}
	random_hour := int(-5 + random_hour_bigint.Int64()) // random number between 1 and 5
	random_min := int(-30 + random_min_bigint.Int64())  // random number between 0 and 59
	time_now := time.Now()
	schedule_time := time.Date(time_now.Year(), time_now.Month(), time_now.Day()+1, random_hour, random_min, 0, 0, time.UTC)
	utils.Log.Println("Scheduled EPG generation on", schedule_time.Local())
	go scheduler.Add(EPG_TASK_ID, time.Until(schedule_time), genepg)
}

// NewProgramme creates a new Programme with the given parameters.
func NewProgramme(channelID int, start, stop, title, desc, pid, category, iconSrc string) Programme {
	iconURL := fmt.Sprintf("%s/%s", EPG_POSTER_URL, iconSrc)
	return Programme{
		Channel: fmt.Sprint(channelID),
		Start:   start,
		Stop:    stop,
		PId:     pid,
		Title: Title{
			Value: title,
			Lang:  "en",
		},
		Desc: Desc{
			Value: desc,
			Lang:  "en",
		},
		Category: Category{
			Value: category,
			Lang:  "en",
		},
		Icon: Icon{
			Src: iconURL,
		},
	}
}

// genXML generates XML EPG from JioTV API and returns it as a byte slice.
func genXML() ([]byte, error) {
	// Create a reusable fasthttp client with common headers
	client := utils.GetRequestClient()

	// Create channels and programmes slices with initial capacity
	var channels []Channel
	var programmes []Programme

	// Define a worker function for fetching EPG data
	fetchEPG := func(channel Channel, bar *progressbar.ProgressBar) {
		req := fasthttp.AcquireRequest()
		req.Header.SetUserAgent("okhttp/4.2.2")
		defer fasthttp.ReleaseRequest(req)

		resp := fasthttp.AcquireResponse()

		for offset := 0; offset < 2; offset++ {
			reqUrl := fmt.Sprintf(EPG_URL, offset, channel.ID)
			req.SetRequestURI(reqUrl)

			if err := client.Do(req, resp); err != nil {
				// Handle error
				utils.Log.Printf("Error fetching EPG for channel %d, offset %d: %v", channel.ID, offset, err)
				continue
			}

			var epgResponse EPGResponse
			if err := json.Unmarshal(resp.Body(), &epgResponse); err != nil {
				// Handle error
				utils.Log.Printf("Error unmarshalling EPG response for channel %d, offset %d: %v", channel.ID, offset, err)
				// Print response body for debugging
				utils.Log.Printf("Response body: %s", resp.Body())
				continue
			}

			for _, programme := range epgResponse.EPG {
				startTime := formatTime(time.UnixMilli(programme.StartEpoch))
				endTime := formatTime(time.UnixMilli(programme.EndEpoch))
				programmeId := strconv.FormatInt(programme.ProgrammeId, 10)
				programmes = append(programmes, NewProgramme(channel.ID, startTime, endTime, programme.Title, programme.Description, programmeId, programme.ShowCategory, programme.Poster))
			}
		}
		bar.Add(1)
		fasthttp.ReleaseResponse(resp)
	}

	// Fetch channels data
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(CHANNEL_URL)
	resp := fasthttp.AcquireResponse()

	utils.Log.Println("Fetching channels")
	if err := client.Do(req, resp); err != nil {
		utils.Log.Fatal(err)
		return nil, err
	}
	defer fasthttp.ReleaseResponse(resp)

	var channelsResponse ChannelsResponse
	if err := json.Unmarshal(resp.Body(), &channelsResponse); err != nil {
		utils.Log.Fatal(err)
		return nil, err
	}

	for _, channel := range channelsResponse.Channels {
		channels = append(channels, Channel{
			ID:      channel.ChannelID,
			Display: channel.ChannelName,
		})
	}
	utils.Log.Println("Fetched", len(channels), "channels")
	// Use a worker pool to fetch EPG data concurrently
	const numWorkers = 20 // Adjust the number of workers based on your needs
	channelQueue := make(chan Channel, len(channels))
	var wg sync.WaitGroup

	// Create a progress bar
	totalChannels := len(channels) // Replace with the actual number of channels
	bar := progressbar.Default(int64(totalChannels))

	utils.Log.Println("Fetching EPG for channels")
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for channel := range channelQueue {
				fetchEPG(channel, bar)
			}
		}()
	}
	// Queue channels for processing
	for _, channel := range channels {
		channelQueue <- channel
	}
	close(channelQueue)
	wg.Wait()

	utils.Log.Println("Fetched programmes")
	// Create EPG and marshal it to XML
	epg := EPG{
		Channel:   channels,
		Programme: programmes,
	}
	xml, err := xml.Marshal(epg)
	if err != nil {
		return nil, err
	}
	return xml, nil
}

// formatTime formats the given time to the string representation "20060102150405 -0700".
func formatTime(t time.Time) string {
	return t.Format("20060102150405 -0700")
}

// prettyFormatXML takes raw XML data and returns a formatted version of the XML.
func prettyFormatXML(rawXML []byte) ([]byte, error) {
	var formattedXML bytes.Buffer
	decoder := xml.NewDecoder(bytes.NewReader(rawXML))
	encoder := xml.NewEncoder(&formattedXML)
	encoder.Indent("", "  ")

	// Tokenize the XML to keep the original structure and formatting
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error decoding XML token: %w", err)
		}

		// Encode the token into the formatted XML buffer
		if err := encoder.EncodeToken(token); err != nil {
			return nil, fmt.Errorf("error encoding XML token: %w", err)
		}
	}
	// Ensure all tokens are flushed to the buffer
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("error flushing XML encoder: %w", err)
	}

	return formattedXML.Bytes(), nil
}

// GenXMLGz generates a gzip file with formatted XML content and an XML header.
func GenXMLGz(filename string) error {
	utils.Log.Println("Generating XML")

	// Assuming genXML() returns a byte slice of raw XML data
	xmlData, err := genXML()
	if err != nil {
		return fmt.Errorf("error generating XML data: %w", err)
	}

	// Add XML header
	xmlHeader := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE tv SYSTEM "http://www.w3.org/2006/05/tv">`

	// Format the raw XML data using the tokenizer method
	formattedXML, err := prettyFormatXML(xmlData)
	if err != nil {
		utils.Log.Println("Failed to format XML. Writing raw XML data instead.")
		formattedXML = xmlData // Fall back to using raw XML data
	}

	// Combine the XML header and the formatted XML data
	var finalXML bytes.Buffer
	finalXML.Write([]byte(xmlHeader + "\n"))
	finalXML.Write(formattedXML)

	// Write the final XML to a gzip file
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer f.Close()

	utils.Log.Println("Writing XML to gzip file")
	gz := gzip.NewWriter(f)
	defer gz.Close()

	// Write the combined XML header and formatted data to the gzip file
	if _, err := gz.Write(finalXML.Bytes()); err != nil {
		return fmt.Errorf("error writing XML data to gzip file: %w", err)
	}

	fmt.Println("\tEPG file generated successfully")
	return nil
}
