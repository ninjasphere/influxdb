package importer

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/influxdb/influxdb/client"
)

const batchSize = 5000

// V8Config is the config used to initialize a V8 importer
type V8Config struct {
	username, password string
	url                url.URL
	precision          string
	writeConsistency   string
	file, version      string
	compressed         bool
}

// NewV8Config returns an initialized *V8Config
func NewV8Config(username, password, precision, writeConsistency, file, version string, u url.URL, compressed bool) *V8Config {
	return &V8Config{
		username:         username,
		password:         password,
		precision:        precision,
		writeConsistency: writeConsistency,
		file:             file,
		version:          version,
		url:              u,
		compressed:       compressed,
	}
}

// V8 is the importer used for importing 0.8 data
type V8 struct {
	client                                     *client.Client
	database                                   string
	retentionPolicy                            string
	config                                     *V8Config
	wg                                         sync.WaitGroup
	line, command                              chan string
	done                                       chan struct{}
	batch                                      []string
	totalInserts, failedInserts, totalCommands int
}

// NewV8 will return an intialized V8 struct
func NewV8(config *V8Config) *V8 {
	return &V8{
		config:  config,
		done:    make(chan struct{}),
		line:    make(chan string),
		command: make(chan string),
		batch:   make([]string, 0, batchSize),
	}
}

// Import processes the specified file in the V8Config and writes the data to the databases in chukes specified by batchSize
func (v8 *V8) Import() error {
	// Create a client and try to connect
	config := client.NewConfig(v8.config.url, v8.config.username, v8.config.password, v8.config.version, client.DEFAULT_TIMEOUT)
	cl, err := client.NewClient(config)
	if err != nil {
		return fmt.Errorf("could not create client %s", err)
	}
	v8.client = cl
	if _, _, e := v8.client.Ping(); e != nil {
		return fmt.Errorf("failed to connect to %s\n", v8.client.Addr())
	}

	// Validate args
	if v8.config.file == "" {
		return fmt.Errorf("file argument required")
	}

	defer func() {
		v8.wg.Wait()
		if v8.totalInserts > 0 {
			log.Printf("Processed %d commands\n", v8.totalCommands)
			log.Printf("Processed %d inserts\n", v8.totalInserts)
			log.Printf("Failed %d inserts\n", v8.failedInserts)
		}
	}()

	// Open the file
	f, err := os.Open(v8.config.file)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader

	// If gzipped, wrap in a gzip reader
	if v8.config.compressed {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gr.Close()
		// Set the reader to the gzip reader
		r = gr
	} else {
		// Standard text file so our reader can just be the file
		r = f
	}

	// start our accumulator
	go v8.batchAccumulator()

	// start our command executor
	go v8.queryExecutor()

	// Get our reader
	scanner := bufio.NewScanner(r)

	// Process the scanner
	v8.processDDL(scanner)
	v8.processDML(scanner)

	// Signal go routines we are done
	close(v8.done)

	// Check if we had any errors scanning the file
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading standard input: %s", err)
	}

	return nil
}

func (v8 *V8) processDDL(scanner *bufio.Scanner) {
	for scanner.Scan() {
		line := scanner.Text()
		// If we find the DML token, we are done with DDL
		if strings.HasPrefix(line, "# DML") {
			return
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		v8.command <- line
	}
}

func (v8 *V8) processDML(scanner *bufio.Scanner) {
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# CONTEXT-DATABASE:") {
			v8.database = strings.TrimSpace(strings.Split(line, ":")[1])
		}
		if strings.HasPrefix(line, "# CONTEXT-RETENTION-POLICY:") {
			v8.retentionPolicy = strings.TrimSpace(strings.Split(line, ":")[1])
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		v8.line <- line
	}
}

func (v8 *V8) execute(command string) {
	response, err := v8.client.Query(client.Query{Command: command, Database: v8.database})
	if err != nil {
		log.Printf("error: %s\n", err)
		return
	}
	if err := response.Error(); err != nil {
		log.Printf("error: %s\n", response.Error())
	}
}

func (v8 *V8) queryExecutor() {
	v8.wg.Add(1)
	defer v8.wg.Done()
	for {
		select {
		case c := <-v8.command:
			v8.totalCommands++
			v8.execute(c)
		case <-v8.done:
			return
		}
	}
}

func (v8 *V8) batchAccumulator() {
	v8.wg.Add(1)
	defer v8.wg.Done()
	for {
		select {
		case l := <-v8.line:
			v8.batch = append(v8.batch, l)
			if len(v8.batch) == batchSize {
				if e := v8.batchWrite(); e != nil {
					log.Println("error writing batch: ", e)
					v8.failedInserts += len(v8.batch)
				} else {
					v8.totalInserts += len(v8.batch)
				}
				v8.batch = v8.batch[:0]
			}
		case <-v8.done:
			v8.totalInserts += len(v8.batch)
			return
		}
	}
}

func (v8 *V8) batchWrite() error {
	_, e := v8.client.WriteLineProtocol(strings.Join(v8.batch, "\n"), v8.database, v8.retentionPolicy, v8.config.precision, v8.config.writeConsistency)
	return e
}
