// Package report provides a mechanism for collecting data on runs and generating a reports and summaries on that data.
package report

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgutz/ansi"
)

// Report captures data for a report/summary.
type Report struct {
	Runs        []*Run
	shouldColor bool
	mu          sync.RWMutex
}

// Run captures data for a run.
type Run struct {
	Started time.Time
	Ended   time.Time
	Reason  *Reason
	Cause   *Cause
	Name    string
	Result  Result
	mu      sync.RWMutex
}

// Result captures the result of a run.
type Result string

// Reason captures the reason for a run.
type Reason string

// Cause captures the cause of a run.
type Cause string

// Summary formats data from a report for output as a summary.
type Summary struct {
	firstRunStart  *time.Time
	lastRunEnd     *time.Time
	padder         string
	TotalUnits     int
	UnitsSucceeded int
	UnitsFailed    int
	EarlyExits     int
	Excluded       int
	shouldColor    bool
}

// Colorizer is a colorizer for the run summary output.
type Colorizer struct {
	headingColorizer     func(string) string
	successColorizer     func(string) string
	failureColorizer     func(string) string
	exitColorizer        func(string) string
	excludeColorizer     func(string) string
	microsecondColorizer func(string) string
	millisecondColorizer func(string) string
	secondColorizer      func(string) string
	minuteColorizer      func(string) string
	defaultColorizer     func(string) string
}

// NewColorizer creates a new Colorizer.
func NewColorizer(shouldColor bool) *Colorizer {
	if !shouldColor {
		return &Colorizer{
			headingColorizer:     func(s string) string { return s },
			successColorizer:     func(s string) string { return s },
			failureColorizer:     func(s string) string { return s },
			exitColorizer:        func(s string) string { return s },
			excludeColorizer:     func(s string) string { return s },
			microsecondColorizer: func(s string) string { return s },
			millisecondColorizer: func(s string) string { return s },
			secondColorizer:      func(s string) string { return s },
			minuteColorizer:      func(s string) string { return s },
			defaultColorizer:     func(s string) string { return s },
		}
	}

	return &Colorizer{
		headingColorizer:     ansi.ColorFunc("yellow+bh"),
		successColorizer:     ansi.ColorFunc("green+bh"),
		failureColorizer:     ansi.ColorFunc("red+bh"),
		exitColorizer:        ansi.ColorFunc("yellow+bh"),
		excludeColorizer:     ansi.ColorFunc("blue+bh"),
		microsecondColorizer: ansi.ColorFunc("cyan+bh"),
		millisecondColorizer: ansi.ColorFunc("cyan+bh"),
		secondColorizer:      ansi.ColorFunc("green+bh"),
		minuteColorizer:      ansi.ColorFunc("yellow+bh"),
		defaultColorizer:     ansi.ColorFunc("white+bh"),
	}
}

// NewReport creates a new report.
func NewReport() *Report {
	report := &Report{
		Runs:        make([]*Run, 0),
		shouldColor: true,
	}

	return report
}

// NewReportOption is an option for creating a new report.
type NewReportOption func(*Report)

// WithDisableColor sets the shouldColor flag for the report.
func (r *Report) WithDisableColor() *Report {
	r.shouldColor = false

	return r
}

// ErrPathMustBeAbsolute is returned when a report run path is not absolute.
var ErrPathMustBeAbsolute = errors.New("report run path must be absolute")

// NewRun creates a new run.
// The path passed in must be an absolute path to ensure that the run can be uniquely identified.
func NewRun(path string) (*Run, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrPathMustBeAbsolute
	}

	return &Run{
		Name:    path,
		Started: time.Now(),
	}, nil
}

// ErrRunAlreadyExists is returned when a run already exists in the report.
var ErrRunAlreadyExists = errors.New("run already exists")

// AddRun adds a run to the report.
// If the run already exists, it returns the ErrRunAlreadyExists error.
func (r *Report) AddRun(run *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existingRun := range r.Runs {
		if existingRun.Name == run.Name {
			return fmt.Errorf("%w: %s", ErrRunAlreadyExists, run.Name)
		}
	}

	r.Runs = append(r.Runs, run)

	return nil
}

// ErrRunNotFound is returned when a run is not found in the report.
var ErrRunNotFound = errors.New("run not found in report")

// GetRun returns a run from the report.
// The path passed in must be an absolute path to ensure that the run can be uniquely identified.
func (r *Report) GetRun(path string) (*Run, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !filepath.IsAbs(path) {
		return nil, ErrPathMustBeAbsolute
	}

	for _, run := range r.Runs {
		if run.Name == path {
			return run, nil
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrRunNotFound, path)
}

// EndRun ends a run and adds it to the report.
// If the run does not exist, it returns the ErrRunNotFound error.
// By default, the run is assumed to have succeeded. To change this, pass WithResult to the function.
func (r *Report) EndRun(path string, endOptions ...EndOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !filepath.IsAbs(path) {
		return ErrPathMustBeAbsolute
	}

	var run *Run

	for _, existingRun := range r.Runs {
		if existingRun.Name == path {
			run = existingRun
			break
		}
	}

	if run == nil {
		return fmt.Errorf("%w: %s", ErrRunNotFound, path)
	}

	run.mu.Lock()
	defer run.mu.Unlock()

	run.Ended = time.Now()
	run.Result = ResultSucceeded

	for _, endOption := range endOptions {
		endOption(run)
	}

	return nil
}

func (r *Report) SortRuns() {
	slices.SortFunc(r.Runs, func(a, b *Run) int {
		return a.Started.Compare(b.Started)
	})
}

// EndOption are optional configurations for ending a run.
type EndOption func(*Run)

const (
	ResultSucceeded Result = "succeeded"
	ResultFailed    Result = "failed"
	ResultEarlyExit Result = "early exit"
	ResultExcluded  Result = "excluded"
)

// WithResult sets the result of a run.
func WithResult(result Result) EndOption {
	return func(run *Run) {
		run.Result = result
	}
}

const (
	ReasonRetrySucceeded Reason = "retry succeeded"
	ReasonErrorIgnored   Reason = "error ignored"
	ReasonRunError       Reason = "run error"
	ReasonExcludeDir     Reason = "--exclude-dir"
	ReasonExcludeBlock   Reason = "exclude block"
	ReasonEarlyExit      Reason = "early exit"
)

// WithReason sets the reason of a run.
func WithReason(reason Reason) EndOption {
	return func(run *Run) {
		run.Reason = &reason
	}
}

// WithCauseRetryBlock sets the cause of a run to the name of a particular retry block.
//
// This function is a wrapper around withCause, just to make sure that authors always use consistent
// reasons for causes.
func WithCauseRetryBlock(name string) EndOption {
	return withCause(name)
}

// WithCauseIgnoreBlock sets the cause of a run to the name of a particular ignore block.
//
// This function is a wrapper around withCause, just to make sure that authors always use consistent
// reasons for causes.
func WithCauseIgnoreBlock(name string) EndOption {
	return withCause(name)
}

// WithCauseExcludeBlock sets the cause of a run to the name of a particular exclude block.
//
// This function is a wrapper around withCause, just to make sure that authors always use consistent
// reasons for causes.
func WithCauseExcludeBlock(name string) EndOption {
	return withCause(name)
}

// WithCauseAncestorExit sets the cause of a run to the name of a particular ancestor that exited.
//
// This function is a wrapper around withCause, just to make sure that authors always use consistent
// reasons for causes.
func WithCauseAncestorExit(name string) EndOption {
	return withCause(name)
}

// withCause sets the cause of a run to the name of a particular cause.
func withCause(name string) EndOption {
	return func(run *Run) {
		cause := Cause(name)
		run.Cause = &cause
	}
}

// These are undocumented temporary environment variables that are used
// to play with the summary, so that we can experiment with it.
const (
	envTmpUndocumentedReportPadder = "TMP_UNDOCUMENTED_REPORT_PADDER"
)

// Summarize returns a summary of the report.
func (r *Report) Summarize() *Summary {
	summary := &Summary{
		TotalUnits:  len(r.Runs),
		shouldColor: r.shouldColor,
		padder:      " ",
	}

	if os.Getenv(envTmpUndocumentedReportPadder) != "" {
		summary.padder = os.Getenv(envTmpUndocumentedReportPadder)
	}

	if len(r.Runs) == 0 {
		return summary
	}

	for _, run := range r.Runs {
		summary.Update(run)
	}

	return summary
}

func (s *Summary) Update(run *Run) {
	run.mu.RLock()
	defer run.mu.RUnlock()

	switch run.Result {
	case ResultSucceeded:
		s.UnitsSucceeded++
	case ResultFailed:
		s.UnitsFailed++
	case ResultEarlyExit:
		s.EarlyExits++
	case ResultExcluded:
		s.Excluded++
	}

	if s.firstRunStart == nil || run.Started.Before(*s.firstRunStart) {
		s.firstRunStart = &run.Started
	}

	if s.lastRunEnd == nil || run.Ended.After(*s.lastRunEnd) {
		s.lastRunEnd = &run.Ended
	}
}

// TotalDuration returns the total duration of all runs in the report.
func (s *Summary) TotalDuration() time.Duration {
	if s.firstRunStart == nil || s.lastRunEnd == nil {
		return 0
	}

	return s.lastRunEnd.Sub(*s.firstRunStart)
}

// TotalDurationString returns the total duration of all runs in the report as a string.
// It returns the duration in the format that is easy to understand by humans.
func (s *Summary) TotalDurationString(colorizer *Colorizer) string {
	duration := s.TotalDuration()

	if duration < time.Millisecond {
		return colorizer.microsecondColorizer(fmt.Sprintf("%dµs", duration.Microseconds()))
	}

	if duration < time.Second {
		return colorizer.millisecondColorizer(fmt.Sprintf("%dms", duration.Milliseconds()))
	}

	if duration < time.Minute {
		return colorizer.secondColorizer(fmt.Sprintf("%ds", int(duration.Seconds())))
	}

	return colorizer.minuteColorizer(fmt.Sprintf("%dm", int(duration.Minutes())))
}

// WriteToFile writes the report to a file.
func (r *Report) WriteToFile(path string) error {
	// Create a temporary file to write to
	tmpFile, err := os.CreateTemp("", "terragrunt-report-*.csv")
	if err != nil {
		return err
	}

	// Sort the runs before writing to the temporary file
	r.mu.Lock()
	r.SortRuns()
	r.mu.Unlock()

	// Write the report to the temporary file
	err = r.WriteCSV(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to write report: %w", err)
	}

	// Close the temporary file
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close report file: %w", err)
	}

	// Move the temporary file to the final destination
	return os.Rename(tmpFile.Name(), path)
}

// WriteCSV writes the report to a writer in CSV format.
func (r *Report) WriteCSV(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	csvWriter := csv.NewWriter(w)
	defer csvWriter.Flush()

	err := csvWriter.Write([]string{
		"Name",
		"Started",
		"Ended",
		"Result",
		"Reason",
		"Cause",
	})
	if err != nil {
		return err
	}

	for _, run := range r.Runs {
		run.mu.RLock()
		defer run.mu.RUnlock()

		name := run.Name
		started := run.Started.Format(time.RFC3339)
		ended := run.Ended.Format(time.RFC3339)
		result := string(run.Result)
		reason := ""

		if run.Reason != nil {
			reason = string(*run.Reason)
		}

		cause := ""
		if run.Cause != nil {
			cause = string(*run.Cause)
		}

		err := csvWriter.Write([]string{
			name,
			started,
			ended,
			result,
			reason,
			cause,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// WriteSummary writes the summary to a writer.
func (r *Report) WriteSummary(w io.Writer) error {
	// Create a line gap before the summary
	_, err := fmt.Fprintf(w, "\n")
	if err != nil {
		return err
	}

	// Write the summary
	err = r.Summarize().Write(w)
	if err != nil {
		return err
	}

	// Write a line gap after the summary
	_, err = fmt.Fprintf(w, "\n")
	if err != nil {
		return err
	}

	return nil
}

// Write writes the summary to a writer.
func (s *Summary) Write(w io.Writer) error {
	colorizer := NewColorizer(s.shouldColor)

	if err := s.writeSummaryHeader(w, colorizer.headingColorizer(runSummaryHeader)); err != nil {
		return err
	}

	if err := s.writeSummaryEntry(w, durationLabel, s.TotalDurationString(colorizer)); err != nil {
		return err
	}

	if err := s.writeSummaryEntry(w, unitsLabel, colorizer.defaultColorizer(strconv.Itoa(s.TotalUnits))); err != nil {
		return err
	}

	if s.UnitsSucceeded > 0 {
		if err := s.writeSummaryEntry(w, successLabel, colorizer.successColorizer(strconv.Itoa(s.UnitsSucceeded))); err != nil {
			return err
		}
	}

	if s.UnitsFailed > 0 {
		if err := s.writeSummaryEntry(w, failureLabel, colorizer.failureColorizer(strconv.Itoa(s.UnitsFailed))); err != nil {
			return err
		}
	}

	if s.EarlyExits > 0 {
		if err := s.writeSummaryEntry(w, earlyExitLabel, colorizer.exitColorizer(strconv.Itoa(s.EarlyExits))); err != nil {
			return err
		}
	}

	if s.Excluded > 0 {
		if err := s.writeSummaryEntry(w, excludeLabel, colorizer.excludeColorizer(strconv.Itoa(s.Excluded))); err != nil {
			return err
		}
	}

	return nil
}

const (
	prefix           = "   "
	runSummaryHeader = "❯❯ Run Summary"
	durationLabel    = "Duration"
	unitsLabel       = "Units"
	successLabel     = "Succeeded"
	failureLabel     = "Failed"
	earlyExitLabel   = "Early Exits"
	excludeLabel     = "Excluded"
	separator        = ": "
)

func (s *Summary) writeSummaryHeader(w io.Writer, value string) error {
	_, err := fmt.Fprintf(w, "%s\n", value)
	if err != nil {
		return err
	}

	return nil
}

func (s *Summary) writeSummaryEntry(w io.Writer, label string, value string) error {
	_, err := fmt.Fprintf(w, "%s%s%s%s %s\n", prefix, label, separator, s.padding(label), value)
	if err != nil {
		return err
	}

	return nil
}

func (s *Summary) longestLineLength() int {
	// Start with the length of the labels
	// That are always present
	lengths := []int{
		len(durationLabel),
		len(unitsLabel),
	}

	// Add the length of the labels that are only present if there are any runs of that type
	if s.UnitsSucceeded > 0 {
		lengths = append(lengths, len(successLabel))
	}

	if s.UnitsFailed > 0 {
		lengths = append(lengths, len(failureLabel))
	}

	if s.EarlyExits > 0 {
		lengths = append(lengths, len(earlyExitLabel))
	}

	if s.Excluded > 0 {
		lengths = append(lengths, len(excludeLabel))
	}

	// Add the length of the entry prefix to each length
	for i, length := range lengths {
		lengths[i] = length + len(prefix)
	}

	// Account for the separator between the label and the value
	for i, length := range lengths {
		lengths[i] = length + len(separator)
	}

	// Return the longest length
	return slices.Max(lengths)
}

func (s *Summary) padding(label string) string {
	longestLineLength := s.longestLineLength()

	labelLength := len(prefix) + len(label) + len(separator)

	return strings.Repeat(s.padder, longestLineLength-labelLength)
}
