package forms

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"log"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/la5nta/wl2k-go/fbb"

	"github.com/la5nta/pat/internal/debug"
)

// Message represents a concrete message compiled from a template
type Message struct {
	To          string      `json:"msg_to"`
	Cc          string      `json:"msg_cc"`
	Subject     string      `json:"msg_subject"`
	Body        string      `json:"msg_body"`
	Attachments []*fbb.File `json:"-"`

	submitted time.Time
}

type messageBuilder struct {
	Interactive bool
	IsReply     bool
	Template    Template
	FormValues  map[string]string
	FormsMgr    *Manager
}

// build returns message subject, body, and attachments for the given template and variable map
func (b messageBuilder) build() (Message, error) {
	b.setDefaultFormValues()
	msg, err := b.scanAndBuild(b.Template.Path)
	if err != nil {
		return Message{}, err
	}
	msg.Attachments = b.buildAttachments()
	return msg, nil
}

func (b messageBuilder) setDefaultFormValues() {
	if b.IsReply {
		b.FormValues["msgisreply"] = "True"
	} else {
		b.FormValues["msgisreply"] = "False"
	}
	for _, key := range []string{"msgsender"} {
		if _, ok := b.FormValues[key]; !ok {
			b.FormValues[key] = b.FormsMgr.config.MyCall
		}
	}

	// some defaults that we can't set yet. Winlink doesn't seem to care about these
	// Set only if they're not set by form values.
	for _, key := range []string{"msgto", "msgcc", "msgsubject", "msgbody", "msgp2p", "txtstr"} {
		if _, ok := b.FormValues[key]; !ok {
			b.FormValues[key] = ""
		}
	}
	for _, key := range []string{"msgisforward", "msgisacknowledgement"} {
		if _, ok := b.FormValues[key]; !ok {
			b.FormValues[key] = "False"
		}
	}

	// TODO: Implement sequences
	for _, key := range []string{"msgseqnum"} {
		if _, ok := b.FormValues[key]; !ok {
			b.FormValues[key] = "0"
		}
	}
}

func (b messageBuilder) buildXML() []byte {
	type Variable struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	}

	filename := func(path string) string {
		// Avoid "." for empty paths
		if path == "" {
			return ""
		}
		return filepath.Base(path)
	}

	form := struct {
		XMLName            xml.Name   `xml:"RMS_Express_Form"`
		XMLFileVersion     string     `xml:"form_parameters>xml_file_version"`
		RMSExpressVersion  string     `xml:"form_parameters>rms_express_version"`
		SubmissionDatetime string     `xml:"form_parameters>submission_datetime"`
		SendersCallsign    string     `xml:"form_parameters>senders_callsign"`
		GridSquare         string     `xml:"form_parameters>grid_square"`
		DisplayForm        string     `xml:"form_parameters>display_form"`
		ReplyTemplate      string     `xml:"form_parameters>reply_template"`
		Variables          []Variable `xml:"variables>name"`
	}{
		XMLFileVersion:     "1.0",
		RMSExpressVersion:  b.FormsMgr.config.AppVersion,
		SubmissionDatetime: now().UTC().Format("20060102150405"),
		SendersCallsign:    b.FormsMgr.config.MyCall,
		GridSquare:         b.FormsMgr.config.Locator,
		DisplayForm:        filename(b.Template.DisplayFormPath),
		ReplyTemplate:      filename(b.Template.ReplyTemplatePath),
	}
	for k, v := range b.FormValues {
		// Trim leading and trailing whitespace. Winlink Express does
		// this, judging from the produced XML attachments.
		v = strings.TrimSpace(v)
		form.Variables = append(form.Variables, Variable{xml.Name{Local: k}, v})
	}
	// Sort vars by name to make sure the output is deterministic.
	sort.Slice(form.Variables, func(i, j int) bool {
		a, b := form.Variables[i], form.Variables[j]
		return a.XMLName.Local < b.XMLName.Local
	})

	data, err := xml.MarshalIndent(form, "", "    ")
	if err != nil {
		panic(err)
	}
	return append([]byte(xml.Header), data...)
}

func (b messageBuilder) buildAttachments() []*fbb.File {
	var attachments []*fbb.File
	// Add optional text attachments defined by some forms as form values
	// pairs in the format attached_textN/attached_fileN (N=0 is omitted).
	for k := range b.FormValues {
		if !strings.HasPrefix(k, "attached_text") {
			continue
		}
		textKey := k
		text := b.FormValues[textKey]
		nameKey := strings.Replace(k, "attached_text", "attached_file", 1)
		name, ok := b.FormValues[nameKey]
		if !ok {
			debug.Printf("%s defined, but corresponding filename element %q is not set", textKey, nameKey)
			name = "FormData.txt" // Fallback (better than nothing)
		}
		attachments = append(attachments, fbb.NewFile(name, []byte(text)))
		delete(b.FormValues, nameKey)
		delete(b.FormValues, textKey)
	}
	// Add XML if a viewer is defined for this template
	if b.Template.DisplayFormPath != "" {
		filename := xmlName(b.Template)
		attachments = append(attachments, fbb.NewFile(filename, b.buildXML()))
	}
	return attachments
}

// scanAndBuild scans the template at the given path, applies placeholder substition and builds the message.
//
// If b,Interactive is true, the user is prompted for undefined placeholders via stdio.
func (b messageBuilder) scanAndBuild(path string) (Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return Message{}, err
	}
	defer f.Close()

	replaceInsertionTags := insertionTagReplacer(b.FormsMgr, "<", ">")
	replaceVars := variableReplacer("<", ">", b.FormValues)
	addFormValue := func(k, v string) {
		b.FormValues[strings.ToLower(k)] = v
		replaceVars = variableReplacer("<", ">", b.FormValues) // Refresh variableReplacer (rebuild regular expressions)
		debug.Printf("Defined %q=%q", k, v)
	}

	scanner := bufio.NewScanner(f)

	msg := Message{submitted: now()}
	var inBody bool
	for scanner.Scan() {
		lineTmpl := scanner.Text()

		// Insertion tags and variables
		lineTmpl = replaceInsertionTags(lineTmpl)
		lineTmpl = replaceVars(lineTmpl)

		// Prompts (mostly found in text templates)
		if b.Interactive {
			lineTmpl = promptAsks(lineTmpl, func(a Ask) string {
				// TODO: Handle a.Multiline as we do message body
				fmt.Printf(a.Prompt + " ")
				ans := b.FormsMgr.config.LineReader()
				if a.Uppercase {
					ans = strings.ToUpper(ans)
				}
				return ans
			})
			lineTmpl = promptSelects(lineTmpl, func(s Select) Option {
				for {
					fmt.Println(s.Prompt)
					for i, opt := range s.Options {
						fmt.Printf("  %d\t%s\n", i, opt.Item)
					}
					fmt.Printf("select 0-%d: ", len(s.Options)-1)
					idx, err := strconv.Atoi(b.FormsMgr.config.LineReader())
					if err == nil && idx < len(s.Options) {
						return s.Options[idx]
					}
				}
			})
			// Fallback prompt for undefined form variables.
			// Typically these are defined by the associated HTML form, but since
			// this is CLI land we'll just prompt for the variable value.
			lineTmpl = promptVars(lineTmpl, func(key string) string {
				fmt.Println(lineTmpl)
				fmt.Printf("%s: ", key)
				value := b.FormsMgr.config.LineReader()
				addFormValue(key, value)
				return value
			})
		}

		if inBody {
			msg.Body += lineTmpl + "\n"
			continue // No control fields in body
		}

		// Control fields
		switch key, value, _ := strings.Cut(lineTmpl, ":"); textproto.CanonicalMIMEHeaderKey(key) {
		case "Msg":
			// The message body starts here. No more control fields after this.
			msg.Body += value
			inBody = true
		case "Form", "ReplyTemplate":
			// Handled elsewhere
			continue
		case "Def", "Define":
			// Def: variable=value – Define the value of a variable.
			key, value, ok := strings.Cut(value, "=")
			if !ok {
				debug.Printf("Def: without key-value pair: %q", value)
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			addFormValue(key, value)
		case "Subject", "Subj":
			// Set the subject of the message
			msg.Subject = strings.TrimSpace(value)
		case "To":
			// Specify to whom the message is being sent
			msg.To = strings.TrimSpace(value)
		case "Cc":
			// Specify carbon copy addresses
			msg.Cc = strings.TrimSpace(value)
		case "Readonly":
			// Yes/No – Specify whether user can edit.
			// TODO: Disable editing of body in composer?
		case "Seqinc":
			// TODO: Handle sequences
		default:
			if strings.TrimSpace(lineTmpl) != "" {
				log.Printf("skipping unknown template line: '%s'", lineTmpl)
			}
		}
	}
	return msg, nil
}

// VariableReplacer returns a function that replaces the given key-value pairs.
func variableReplacer(tagStart, tagEnd string, vars map[string]string) func(string) string {
	return placeholderReplacer(tagStart+"Var ", tagEnd, vars)
}

// InsertionTagReplacer returns a function that replaces the fixed set of insertion tags with their corresponding values.
func insertionTagReplacer(m *Manager, tagStart, tagEnd string) func(string) string {
	now := now()
	validPos := "NO"
	nowPos, err := m.gpsPos()
	if err != nil {
		debug.Printf("GPSd error: %v", err)
	} else {
		validPos = "YES"
		debug.Printf("GPSd position: %s", positionFmt(signedDecimal, nowPos))
	}
	// This list is based on RMSE_FORMS/insertion_tags.zip (copy in docs/) as well as searching Standard Forms's templates.
	return placeholderReplacer(tagStart, tagEnd, map[string]string{
		"MsgSender":      m.config.MyCall,
		"Callsign":       m.config.MyCall,
		"ProgramVersion": m.config.AppVersion,

		"DateTime":  formatDateTime(now),
		"UDateTime": formatDateTimeUTC(now),
		"Date":      formatDate(now),
		"UDate":     formatDateUTC(now),
		"UDTG":      formatUDTG(now),
		"Time":      formatTime(now),
		"UTime":     formatTimeUTC(now),
		"Day":       formatDay(now, location),
		"UDay":      formatDay(now, time.UTC),

		"GPS":                positionFmt(degreeMinute, nowPos),
		"GPSValid":           validPos,
		"GPS_DECIMAL":        positionFmt(decimal, nowPos),
		"GPS_SIGNED_DECIMAL": positionFmt(signedDecimal, nowPos),
		"GridSquare":         positionFmt(gridSquare, nowPos),
		"Latitude":           fmt.Sprintf("%.4f", nowPos.Lat),
		"Longitude":          fmt.Sprintf("%.4f", nowPos.Lon),
		// No docs found for these, but they are referenced by a couple of templates in Standard Forms.
		// By reading the embedded javascript, they appear to be signed decimal.
		"GPSLatitude":  fmt.Sprintf("%.4f", nowPos.Lat),
		"GPSLongitude": fmt.Sprintf("%.4f", nowPos.Lon),

		// TODO (other insertion tags found in Standard Forms):
		// SeqNum
		// FormFolder
		// InternetAvailable
		// MsgTo
		// MsgCc
		// MsgSubject
		// MsgP2P
		// Sender (only in 'ARC Forms/Disaster Receipt 6409-B Reply.0')
		// Speed  (only in 'GENERAL Forms/GPS Position Report.txt' - but not included in produced message body)
		// course (only in 'GENERAL Forms/GPS Position Report.txt' - but not included in produced message body)
		// decimal_separator

		// TODO: MsgOriginal* (see "RMSE_FORMS/insertion_tags.zip/Insertion Tags.txt")
		//       This will require changing the IsReply/composereply boolean to a message reference.
	})
}

// xmlName returns the user-visible filename for the message attachment that holds the form instance values
func xmlName(t Template) string {
	attachmentName := filepath.Base(t.DisplayFormPath)
	attachmentName = strings.TrimSuffix(attachmentName, filepath.Ext(attachmentName))
	attachmentName = "RMS_Express_Form_" + attachmentName + ".xml"
	if len(attachmentName) > 255 {
		attachmentName = strings.TrimPrefix(attachmentName, "RMS_Express_Form_")
	}
	return attachmentName
}
