package process_request

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	armoUtils "github.com/armosec/utils-go/httputils"

	"github.com/hashicorp/go-multierror"

	// "ca-vuln-scan/catypes"

	"log"
	"os"

	wssc "github.com/armosec/armoapi-go/apis"
	"github.com/armosec/armoapi-go/armotypes"
	sysreport "github.com/armosec/logger-go/system-reports/datastructures"
	"k8s.io/utils/strings/slices"

	cs "github.com/armosec/cluster-container-scanner-api/containerscan"
)

var ociClient OcimageClient
var eventRecieverURL string
var cusGUID string
var printPostJSON string

//
func init() {
	ociClient.endpoint = os.Getenv("OCIMAGE_URL")
	if len(ociClient.endpoint) == 0 {
		ociClient.endpoint = os.Getenv("CA_OCIMAGE_URL")
		if len(ociClient.endpoint) == 0 {
			log.Printf("OCIMAGE_URL/CA_OCIMAGE_URL is not configured- some features might not work, please install OCIMAGE to get more features")
		}
	}

	eventRecieverURL = os.Getenv("CA_EVENT_RECEIVER_HTTP")
	if len(eventRecieverURL) == 0 {
		log.Fatal("Must configure either CA_EVENT_RECEIVER_HTTP")
	}

	cusGUID = os.Getenv("CA_CUSTOMER_GUID")
	if len(cusGUID) == 0 {
		log.Fatal("Must configure CA_CUSTOMER_GUID")
	}
	printPostJSON = os.Getenv("PRINT_POST_JSON")
}

func getContainerImageManifest(scanCmd *wssc.WebsocketScanCommand) (*OciImageManifest, error) {
	oci := OcimageClient{endpoint: "http://localhost:8080"}
	image, err := oci.Image(scanCmd)
	if err != nil {
		return nil, err
	}
	manifest, err := image.GetManifest()
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func (oci *OcimageClient) GetContainerImage(scanCmd *wssc.WebsocketScanCommand) (*OciImage, error) {
	image, err := oci.Image(scanCmd)
	if err != nil {
		return nil, err
	}
	return image, nil
}

const maxBodySize int = 30000

func postScanResultsToEventReciever(scanCmd *wssc.WebsocketScanCommand, imagetag, imageHash string, wlid string, containerName string, layersList *cs.LayersList, listOfBash []string) error {

	log.Printf("posting to event reciever image %s wlid %s", imagetag, wlid)
	timestamp := int64(time.Now().Unix())

	final_report := cs.ScanResultReport{
		CustomerGUID:             cusGUID,
		ImgTag:                   imagetag,
		ImgHash:                  imageHash,
		WLID:                     wlid,
		ContainerName:            containerName,
		Timestamp:                timestamp,
		Layers:                   *layersList,
		ListOfDangerousArtifcats: listOfBash,
		Session:                  scanCmd.Session,
		Designators: armotypes.PortalDesignator{
			Attributes: map[string]string{},
		},
	}
	if val, ok := scanCmd.Args[armotypes.AttributeRegistryName]; ok {
		final_report.Designators.Attributes[armotypes.AttributeRegistryName] = val.(string)
	}

	if val, ok := scanCmd.Args[armotypes.AttributeRepository]; ok {
		final_report.Designators.Attributes[armotypes.AttributeRepository] = val.(string)
	}

	if val, ok := scanCmd.Args[armotypes.AttributeTag]; ok {
		final_report.Designators.Attributes[armotypes.AttributeTag] = val.(string)
	}

	log.Printf("session: %v\n===\n", final_report.Session)
	//split vulnerabilities to chunks
	chunksChan, totalVulnerabilities := splitVulnerabilities2Chunks(final_report.ToFlatVulnerabilities(), maxBodySize)
	//send report(s)
	scanID := final_report.AsFNVHash()
	sendWG := &sync.WaitGroup{}
	errChan := make(chan error, 10)
	//get the first chunk
	firstVulnerabilitiesChunk := <-chunksChan
	firstChunkVulnerabilitiesCount := len(firstVulnerabilitiesChunk)
	firstVulnerabilitiesChunk = nil
	//send the summery and the first chunk in one or two reports according to the size
	nextPartNum := sendSummeryAndVulnerabilities(final_report, totalVulnerabilities, scanID, firstVulnerabilitiesChunk, errChan, sendWG)
	//if not all vulnerabilities got into the first chunk
	if totalVulnerabilities != firstChunkVulnerabilitiesCount {
		//send the rest of the vulnerabilities
		sendVulnerabilitiesRoutine(chunksChan, scanID, final_report, errChan, sendWG, totalVulnerabilities, firstChunkVulnerabilitiesCount, nextPartNum)
	}
	//collect post report errors if occurred
	var err error
	for e := range errChan {
		err = multierror.Append(err, e)
	}
	return err
}

func sendVulnerabilitiesRoutine(chunksChan <-chan []cs.CommonContainerVulnerabilityResult, scanID string, final_report cs.ScanResultReport, errChan chan error, sendWG *sync.WaitGroup, totalVulnerabilities int, firstChunkVulnerabilitiesCount int, nextPartNum int) {
	go func(scanID string, final_report cs.ScanResultReport, errorChan chan<- error, sendWG *sync.WaitGroup, expectedVulnerabilitiesSum int, partNum int) {
		sendVulnerabilities(chunksChan, partNum, expectedVulnerabilitiesSum, scanID, final_report, errorChan, sendWG)
	}(scanID, final_report, errChan, sendWG, totalVulnerabilities-firstChunkVulnerabilitiesCount, nextPartNum)
}

func sendVulnerabilities(chunksChan <-chan []cs.CommonContainerVulnerabilityResult, partNum int, expectedVulnerabilitiesSum int, scanID string, final_report cs.ScanResultReport, errorChan chan<- error, sendWG *sync.WaitGroup) {
	//post each vulnerabilities chunk in a different report
	chunksVulnerabilitiesCount := 0
	for vulnerabilities := range chunksChan {
		chunksVulnerabilitiesCount += len(vulnerabilities)
		postResultsAsGoroutine(&cs.ScanResultReportV1{
			PartNum:         partNum,
			LastPart:        chunksVulnerabilitiesCount == expectedVulnerabilitiesSum,
			Vulnerabilities: vulnerabilities,
			ContainerScanID: scanID,
			CustomerGUID:    final_report.CustomerGUID,
			Timestamp:       final_report.Timestamp,
			Designators:     final_report.Designators,
			WLID:            final_report.WLID,
			ContainerName:   final_report.ContainerName,
		}, final_report.ImgTag, final_report.WLID, errorChan, sendWG)
		partNum++
	}

	//verify that all vulnerabilities received and sent
	if chunksVulnerabilitiesCount != expectedVulnerabilitiesSum {
		errorChan <- fmt.Errorf("error while splitting vulnerabilities chunks, expected " + strconv.Itoa(expectedVulnerabilitiesSum) +
			" vulnerabilities but received " + strconv.Itoa(chunksVulnerabilitiesCount))
	}
	sendWG.Wait()

	close(errorChan)
}

func sendSummeryAndVulnerabilities(report cs.ScanResultReport, totalVulnerabilities int, scanID string, firstVulnerabilitiesChunk []cs.CommonContainerVulnerabilityResult, errChan chan<- error, sendWG *sync.WaitGroup) (nextPartNum int) {
	//get the first chunk
	firstChunkVulnerabilitiesCount := len(firstVulnerabilitiesChunk)
	//prepare summery report
	nextPartNum = 1
	summeryReport := &cs.ScanResultReportV1{
		PartNum:         nextPartNum,
		LastPart:        totalVulnerabilities == firstChunkVulnerabilitiesCount,
		Summery:         report.Summarize(),
		ContainerScanID: scanID,
		CustomerGUID:    report.CustomerGUID,
		Timestamp:       report.Timestamp,
		Designators:     report.Designators,
		WLID:            report.WLID,
		ContainerName:   report.ContainerName,
	}
	//if size of summery + first chunk does not exceed max size
	if armoUtils.JSONSize(summeryReport)+armoUtils.JSONSize(firstVulnerabilitiesChunk) <= maxBodySize {
		//then post the summary report with the first vulnerabilities chunk
		summeryReport.Vulnerabilities = firstVulnerabilitiesChunk
		//if all vulnerabilities got into the first chunk set this as the last report
		summeryReport.LastPart = totalVulnerabilities == firstChunkVulnerabilitiesCount
		//first chunk sent (or is nil) so set to nil
		firstVulnerabilitiesChunk = nil
	} else {
		//first chunk is not included in the summery, so if there are vulnerabilities to send set the last part to false
		summeryReport.LastPart = firstChunkVulnerabilitiesCount < 0
	}
	//send the summery report
	postResultsAsGoroutine(summeryReport, report.ImgTag, report.WLID, errChan, sendWG)
	//free memory
	summeryReport = nil
	nextPartNum++
	//send the first chunk if it was not sent yet (because of summery size)
	if firstVulnerabilitiesChunk != nil {
		postResultsAsGoroutine(&cs.ScanResultReportV1{
			PartNum:         nextPartNum,
			LastPart:        totalVulnerabilities == firstChunkVulnerabilitiesCount,
			Vulnerabilities: firstVulnerabilitiesChunk,
			ContainerScanID: scanID,
			CustomerGUID:    report.CustomerGUID,
			Timestamp:       report.Timestamp,
			Designators:     report.Designators,
			WLID:            report.WLID,
			ContainerName:   report.ContainerName,
		}, report.ImgTag, report.WLID, errChan, sendWG)
		nextPartNum++
	}
	return nextPartNum
}

func splitVulnerabilities2Chunks(vulnerabilities []cs.CommonContainerVulnerabilityResult, maxChunkSize int) (chunksChan <-chan []cs.CommonContainerVulnerabilityResult, totalVulnerabilities int) {
	//split vulnerabilities to chunks
	channel := make(chan []cs.CommonContainerVulnerabilityResult, 10)
	totalVulnerabilities = len(vulnerabilities)
	if totalVulnerabilities > 0 {
		go func(vulnerabilities []cs.CommonContainerVulnerabilityResult, chunksChan chan<- []cs.CommonContainerVulnerabilityResult) {
			splitWg := &sync.WaitGroup{}
			armoUtils.SplitSlice2Chunks(vulnerabilities, maxChunkSize, chunksChan, splitWg)
			splitWg.Wait()
			//done splitting - close the chunks channel
			close(channel)
		}(vulnerabilities, channel)
		//free memory
		vulnerabilities = nil
	} else {
		//no vulnerabilities just close the channel
		close(channel)
	}
	return channel, totalVulnerabilities
}

func postResultsAsGoroutine(report *cs.ScanResultReportV1, imagetag string, wlid string, errorChan chan<- error, wg *sync.WaitGroup) {
	wg.Add(1)
	go func(report *cs.ScanResultReportV1, imagetag string, wlid string, errorChan chan<- error, wg *sync.WaitGroup) {
		defer wg.Done()
		postResults(report, imagetag, wlid, errorChan)
	}(report, imagetag, wlid, errorChan, wg)

}
func postResults(report *cs.ScanResultReportV1, imagetag string, wlid string, errorChan chan<- error) {
	payload, err := json.Marshal(report)
	if err != nil {
		log.Printf("fail convert to json")
		errorChan <- err
		return
	}
	if printPostJSON != "" {
		log.Printf("printPostJSON:")
		log.Printf("%v", string(payload))
	}
	resp, err := http.Post(eventRecieverURL+"/k8s/containerScanV1?CustomerGUID="+report.CustomerGUID, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("fail posting to event receiver image %s wlid %s", imagetag, wlid)
		errorChan <- err
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || 299 < resp.StatusCode {
		log.Printf("Vulnerabilities post to event receiver failed with %d", resp.StatusCode)
		errorChan <- err
		return
	}
	log.Printf("posting to event receiver image %s wlid %s finished successfully", imagetag, wlid)
}

func RemoveFile(filename string) {
	err := os.Remove(filename)
	if err != nil {
		log.Printf("Error removing file %s", filename)
	}
}

func GetScanResult(scanCmd *wssc.WebsocketScanCommand) (*cs.LayersList, []string, error) {
	filteredResultsChan := make(chan []string)

	/*
		This code get list of executables that can be dangerous
	*/
	if ociClient.endpoint != "" {
		ociImage, err := ociClient.Image(scanCmd)
		if err != nil {
			log.Printf("unable to get image %s", err)
			go func() {
				log.Printf("skipping dangerous executables enrichment")
				filteredResultsChan <- nil
			}()
			// return nil, nil, err
		} else {
			go func() {
				listOfPrograms := []string{
					"bin/sh", "bin/bash", "sbin/sh", "bin/ksh", "bin/tcsh", "bin/zsh", "usr/bin/scsh", "bin/csh", "bin/busybox", "usr/bin/kubectl", "usr/bin/curl",
					"usr/bin/wget", "usr/bin/ssh", "usr/bin/ftp", "usr/share/gdb", "usr/bin/nmap", "usr/share/nmap", "usr/bin/tcpdump", "usr/bin/ping",
					"usr/bin/netcat", "usr/bin/gcc", "usr/bin/busybox", "usr/bin/nslookup", "usr/bin/host", "usr/bin/dig", "usr/bin/psql", "usr/bin/swaks",
				}
				filteredResult := []string{}

				directoryFilesInBytes, err := ociImage.GetFiles(listOfPrograms, true, true)
				if err != nil {
					log.Printf("Couldn't get filelist from ocimage  due to %s", err.Error())
					filteredResultsChan <- nil
					return
				}
				rand.Seed(time.Now().UnixNano())
				randomNum := rand.Intn(100)
				filename := "/tmp/file" + fmt.Sprint(randomNum) + ".tar.gz"
				permissions := 0644
				ioutil.WriteFile(filename, *directoryFilesInBytes, fs.FileMode(permissions))

				reader, err := os.Open(filename)
				if err != nil {
					log.Printf("Couldn't open file : %s" + filename)
					filteredResultsChan <- nil
					return
				}
				defer reader.Close()
				defer RemoveFile(filename)

				tarReader := tar.NewReader(reader)
				buf := new(strings.Builder)
				for {
					currentFile, err := tarReader.Next()
					if err == io.EOF {
						break
					}

					if currentFile.Name == "symlinkMap.json" {
						_, err := io.Copy(buf, tarReader)
						if err != nil {
							log.Printf("Couldn't parse symlinkMap.json file")
							filteredResultsChan <- nil
							return
						}

					}
				}
				var fileInJson map[string]string
				err = json.Unmarshal([]byte(buf.String()), &fileInJson)
				if err != nil {
					log.Printf("Failed to marshal file  %s", filename)
					filteredResultsChan <- nil
					return
				}

				for _, element := range listOfPrograms {
					if element, ok := fileInJson[element]; ok {
						filteredResult = append(filteredResult, element)
					}
				}
				filteredResultsChan <- filteredResult
			}()
		}
	} else {
		go func() {
			log.Printf("skipping dangerous executables enrichment")
			filteredResultsChan <- nil
		}()

	}
	/*
		End of dangerous execuatables collect code
	*/

	scanresultlayer, err := GetAnchoreScanResults(scanCmd)
	if err != nil {
		log.Printf("%v", err.Error())
		return nil, nil, err
	}

	filteredResult := <-filteredResultsChan

	return scanresultlayer, filteredResult, nil
}

func ProcessScanRequest(scanCmd *wssc.WebsocketScanCommand) (*cs.LayersList, error) {
	report := &sysreport.BaseReport{
		CustomerGUID: os.Getenv("CA_CUSTOMER_GUID"),
		Reporter:     "ca-vuln-scan",
		Status:       sysreport.JobStarted,
		Target: fmt.Sprintf("vuln scan:: scanning wlid: %v , container: %v imageTag: %v imageHash: %s", scanCmd.Wlid,
			scanCmd.ContainerName, scanCmd.ImageTag, scanCmd.ImageHash),
		ActionID:     "2",
		ActionIDN:    2,
		ActionName:   "vuln scan",
		JobID:        scanCmd.JobID,
		ParentAction: scanCmd.ParentJobID,
		Details:      "Dequeueing",
	}

	if len(scanCmd.JobID) != 0 {
		report.SetJobID(scanCmd.JobID)
	}
	if scanCmd.LastAction > 0 {
		report.SetActionIDN(scanCmd.LastAction + 1)
	}

	jobID := report.GetJobID()
	if !slices.Contains(scanCmd.Session.JobIDs, jobID) {
		scanCmd.Session.JobIDs = append(scanCmd.Session.JobIDs, jobID)
	}

	report.SendAsRoutine([]string{}, true)
	// NewBaseReport(cusGUID, )
	result, bashList, err := GetScanResult(scanCmd)
	if err != nil {

		report.SendError(err, true, true)
		return nil, err
	}
	report.SendStatus(sysreport.JobSuccess, true)
	report.SendAction(fmt.Sprintf("vuln scan:notifying event receiver about %v scan", scanCmd.ImageTag), true)

	//Benh - dangerous hack

	err = postScanResultsToEventReciever(scanCmd, scanCmd.ImageTag, scanCmd.ImageHash, scanCmd.Wlid, scanCmd.ContainerName, result, bashList)
	if err != nil {
		report.SendError(fmt.Errorf("vuln scan:notifying event receiver about %v scan failed due to %v", scanCmd.ImageTag, err.Error()), true, true)
	} else {
		report.SendStatus(sysreport.JobDone, true)
	}
	return result, nil
}
