package collector

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/boynux/squid-exporter/types"
	proxyproto "github.com/pires/go-proxyproto"
)

/*CacheObjectClient holds information about squid manager */
type CacheObjectClient struct {
	hostname          string
	port              int
	basicAuthString   string
	headers           map[string]string
	withProxyProtocal bool
}

/*SquidClient provides functionality to fetch squid metrics */
type SquidClient interface {
	GetCounters() (types.Counters, error)
	GetServiceTimes() (types.Counters, error)
}

const (
	requestProtocol = "GET cache_object://localhost/%s HTTP/1.0"
)

func buildBasicAuthString(login string, password string) string {
	if len(login) == 0 {
		return ""
	} else {
		return base64.StdEncoding.EncodeToString([]byte(login + ":" + password))
	}
}

/*NewCacheObjectClient initializes a new cache client */
func NewCacheObjectClient(hostname string, port int, login string, password string, withProxyProtocal bool) *CacheObjectClient {
	return &CacheObjectClient{
		hostname,
		port,
		buildBasicAuthString(login, password),
		map[string]string{},
		withProxyProtocal,
	}
}

func readFromSquid(hostname string, port int, basicAuthString string, endpoint string, withProxyProtocal bool) (*bufio.Reader, error) {
	conn, err := connect(hostname, port)

	if err != nil {
		return nil, err
	}

	if withProxyProtocal {
		// set proxy proto header (version 1)
		// from: localhost:80
		// to: localhost: <port>
		header := &proxyproto.Header{
			Version:           1,
			Command:           proxyproto.PROXY,
			TransportProtocol: proxyproto.TCPv4,
			SourceAddr: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: 80,
			},

			DestinationAddr: &net.TCPAddr{
				IP:   net.ParseIP("127.0.0.1"),
				Port: port,
			},
		}
		// After the connection was created write the proxy headers first
		_, err = header.WriteTo(conn)

		if err != nil {
			return nil, err
		}
	}

	r, err := get(conn, endpoint, basicAuthString)

	if err != nil {
		return nil, err
	}

	if r.StatusCode != 200 {
		return nil, fmt.Errorf("Non success code %d while fetching metrics", r.StatusCode)
	}

	return bufio.NewReader(r.Body), err
}

func readLines(reader *bufio.Reader, lines chan<- string) {
	for {
		line, err := reader.ReadString('\n')

		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("error reading from the bufio.Reader: %v", err)
			break
		}

		lines <- line
	}
	close(lines)
}

/*GetCounters fetches counters from squid cache manager */
func (c *CacheObjectClient) GetCounters() (types.Counters, error) {
	var counters types.Counters

	reader, err := readFromSquid(c.hostname, c.port, c.basicAuthString, "counters", c.withProxyProtocal)
	if err != nil {
		return nil, fmt.Errorf("error getting counters: %v", err)
	}

	lines := make(chan string)
	go readLines(reader, lines)

	for line := range lines {
		c, err := decodeCounterStrings(line)
		if err != nil {
			log.Println(err)
		} else {
			counters = append(counters, c)
		}
	}

	return counters, err
}

/*GetServiceTimes fetches service times from squid cache manager */
func (c *CacheObjectClient) GetServiceTimes() (types.Counters, error) {
	var serviceTimes types.Counters

	reader, err := readFromSquid(c.hostname, c.port, c.basicAuthString, "service_times", c.withProxyProtocal)
	if err != nil {
		return nil, fmt.Errorf("error getting service times: %v", err)
	}

	lines := make(chan string)
	go readLines(reader, lines)

	for line := range lines {
		s, err := decodeServiceTimeStrings(line)
		if err != nil {
			log.Println(err)
		} else {
			if s.Key != "" {
				serviceTimes = append(serviceTimes, s)
			}
		}
	}

	return serviceTimes, err
}

func connect(hostname string, port int) (net.Conn, error) {
	return net.Dial("tcp", fmt.Sprintf("%s:%d", hostname, port))
}

func get(conn net.Conn, path string, basicAuthString string) (*http.Response, error) {
	rBody := []string{
		fmt.Sprintf(requestProtocol, path),
		"Host: localhost",
		"User-Agent: squidclient/3.5.12",
	}
	if len(basicAuthString) > 0 {
		rBody = append(rBody, "Proxy-Authorization: Basic "+basicAuthString)
	}
	rBody = append(rBody, "Accept: */*", "\r\n")
	request := strings.Join(rBody, "\r\n")

	fmt.Fprintf(conn, request)
	return http.ReadResponse(bufio.NewReader(conn), nil)
}

func decodeCounterStrings(line string) (types.Counter, error) {
	if equal := strings.Index(line, "="); equal >= 0 {
		if key := strings.TrimSpace(line[:equal]); len(key) > 0 {
			value := ""
			if len(line) > equal {
				value = strings.TrimSpace(line[equal+1:])
			}

			// Remove additional formating string from `sample_time`
			if slices := strings.Split(value, " "); len(slices) > 0 {
				value = slices[0]
			}

			if i, err := strconv.ParseFloat(value, 64); err == nil {
				return types.Counter{Key: key, Value: i}, nil
			}
		}
	}

	return types.Counter{}, errors.New("counter - could not parse line: " + line)
}

func decodeServiceTimeStrings(line string) (types.Counter, error) {
	if strings.HasSuffix(line, ":\n") { // A header line isn't a metric
		return types.Counter{}, nil
	}
	if equal := strings.Index(line, ":"); equal >= 0 {
		if key := strings.TrimSpace(line[:equal]); len(key) > 0 {
			value := ""
			if len(line) > equal {
				value = strings.TrimSpace(line[equal+1:])
			}
			key = strings.Replace(key, " ", "_", -1)
			key = strings.Replace(key, "(", "", -1)
			key = strings.Replace(key, ")", "", -1)

			if equalTwo := strings.Index(value, "%"); equalTwo >= 0 {
				if keyTwo := strings.TrimSpace(value[:equalTwo]); len(keyTwo) > 0 {
					if len(value) > equalTwo {
						value = strings.Split(strings.TrimSpace(value[equalTwo+1:]), " ")[0]
					}
					key = key + "_" + keyTwo
				}
			}

			if value, err := strconv.ParseFloat(value, 64); err == nil {
				return types.Counter{Key: key, Value: value}, nil
			}
		}
	}

	return types.Counter{}, errors.New("service times - could not parse line: " + line)
}
