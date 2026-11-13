package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ddlg3003/d-mock/internal/scenario"
)

// JUnit XML, the test report format CI systems (GitLab, GitHub Actions, Jenkins) display:
// one <testsuite> per d-mock suite, one <testcase> per scenario.
type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     secs        `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      secs          `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// secs writes seconds with millisecond precision (a float sum prints as 13.825000000000001).
type secs float64

func (v secs) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	return xml.Attr{Name: name, Value: fmt.Sprintf("%.3f", float64(v))}, nil
}

func writeJUnit(path, suiteName string, reports []*scenario.Report) error {
	s := junitSuite{Name: suiteName, Tests: len(reports)}
	for _, r := range reports {
		d, _ := time.ParseDuration(r.Duration)
		s.Time += secs(d.Seconds())
		tc := junitCase{Name: r.Scenario, Classname: r.File, Time: secs(d.Seconds())}
		if r.Skipped { // never ran (before_all failed, interrupted)
			s.Skipped++
			tc.Skipped = &junitSkipped{Message: r.Steps[0].Name}
			s.Cases = append(s.Cases, tc)
			continue
		}
		var out, fail []string
		for _, st := range append(append([]scenario.StepResult{}, r.Steps...), r.Cleanup...) {
			mark := "✓"
			switch {
			case st.Skipped:
				mark = "-"
			case !st.Passed:
				mark = "✗"
				fail = append(fail, st.Name)
				if st.Error != "" {
					fail = append(fail, "  error: "+st.Error)
				}
				for _, f := range st.Failures {
					fail = append(fail, "  expect: "+f)
				}
			}
			out = append(out, mark+" "+st.Name+" ["+st.Type+"] "+st.Duration)
		}
		tc.SystemOut = "run=" + r.RunID + "\n" + strings.Join(out, "\n")
		if !r.Passed {
			s.Failures++
			msg := "failed"
			if len(fail) > 0 {
				msg = "failed at: " + fail[0]
			}
			tc.Failure = &junitFailure{Message: msg, Text: strings.Join(fail, "\n")}
		}
		s.Cases = append(s.Cases, tc)
	}
	b, err := xml.MarshalIndent(junitSuites{Suites: []junitSuite{s}}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(xml.Header), append(b, '\n')...), 0o644)
}
