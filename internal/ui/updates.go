package ui

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/update"
)

// Updates is the update subsystem as the desktop UI uses it. Status and
// channel selection are local operations; Check and Download are explicit
// network actions, and Install hands a verified candidate to the replacement
// helper.
type Updates interface {
	Status() update.Status
	SetChannel(update.Channel) error
	Check() error
	Download(tag string) error
	Install() error
}

// UpdatesView presents the updater's local state without becoming another
// update authority.
type UpdatesView struct {
	update.Status
	Channels          []update.Channel
	Op, LastOp        *updateOperationView
	OpHint, CheckHint string
	ReadyHint         string
	StateHint         string
	ResultHint        string
	OpOutcome         []string
}

type updateOperationView struct {
	update.Operation
	Determinate bool
	Percent     float64
	PhaseNumber int
	PhaseCount  int
	Stages      []updateStage
}

type updateStage struct {
	Label  string
	Status string
}

type updatesPageData struct {
	Locale    i18n.Locale
	Token     string
	Error     string
	ErrorHint string
	Upd       *UpdatesView
}

//go:embed updates.html
var updatesHTML string

var updatesTemplate = template.Must(template.New("updates").Funcs(updatesTemplateFuncs()).Parse(updatesHTML))

func updatesTemplateFuncs() template.FuncMap {
	funcs := template.FuncMap{
		"systemCSS":  CSS,
		"when":       updateTime,
		"bytesIn":    updateBytes,
		"phaseLabel": updatePhaseLabel,
		"stepLabel":  updateStepLabel,
		"stageWord":  updateStageWord,
		"opLabel":    updateOperationLabel,
	}
	for name, fn := range templateFuncs() {
		funcs[name] = fn
	}
	return funcs
}

func (h *handler) updatesView() *UpdatesView {
	if h.updates == nil {
		return nil
	}
	status := h.updates.Status()
	v := &UpdatesView{Status: status, Channels: update.Channels, Op: updateOperation(status.Busy), LastOp: updateOperation(status.Last)}
	if status.Last != nil && status.Last.Failure != nil {
		v.OpHint = update.Hints[status.Last.Failure.Class]
		v.OpOutcome = downloadOutcome(status.Last.Failure, status)
	}
	if status.LastCheck != nil && !status.LastCheck.OK {
		v.CheckHint = update.Hints[status.LastCheck.Class]
	}
	if status.ReadyProblem != "" {
		v.ReadyHint = "This candidate is not installable. Review the status and error detail above, then check for updates or download another verified candidate."
	}
	if status.Err != "" {
		v.StateHint = "The updater could not read or save local update state. Check Matagi's state directory and review the error detail."
	}
	if status.Result != nil {
		switch status.Result.Outcome {
		case update.OutcomeFailed:
			v.ResultHint = update.Hints[update.ClassReplace]
		case update.OutcomeRestart:
			v.ResultHint = update.Hints[update.ClassRestart]
		case update.OutcomeRefused:
			v.ResultHint = "The update attempt was refused. Review the current update status and error detail before retrying."
		}
	}
	return v
}

func updateOperation(op *update.Operation) *updateOperationView {
	if op == nil {
		return nil
	}
	v := &updateOperationView{Operation: *op, Determinate: op.Total > 0}
	if v.Determinate {
		v.Percent = updatePercent(op.Done, op.Total)
	}
	plan := op.Plan
	if len(plan) == 0 {
		plan = op.Phases
	}
	if phaseIndex := indexOf(plan, op.Phase); phaseIndex >= 0 {
		v.PhaseNumber = phaseIndex + 1
		v.PhaseCount = len(plan)
	}
	v.Stages = make([]updateStage, 0, len(plan))
	for _, phase := range plan {
		stage := updateStage{Label: updatePhaseLabel(phase), Status: "pending"}
		seen := indexOf(op.Phases, phase)
		switch {
		case seen < 0 && !op.Finished.IsZero() && op.Failure == nil:
			stage.Status = "skipped"
		case seen < 0:
		case op.Failure != nil && phase == op.Failure.Phase:
			stage.Status = "failed"
		case op.Finished.IsZero() && phase == op.Phase:
			stage.Status = "current"
		default:
			stage.Status = "done"
		}
		v.Stages = append(v.Stages, stage)
	}
	return v
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func updatePhaseLabel(phase string) string {
	switch phase {
	case update.PhaseReleases:
		return "Release list"
	case update.PhaseChecksum:
		return "Checksum file"
	case update.PhaseDownload:
		return "Download"
	case update.PhaseVerify:
		return "Verification"
	default:
		return phase
	}
}

func updateStepLabel(step string) string {
	switch step {
	case update.StepFetching:
		return "Fetching"
	case update.StepDownloading:
		return "Downloading"
	case update.StepVerifying:
		return "Verifying"
	default:
		return step
	}
}

func updateStageWord(status string) string {
	switch status {
	case "done":
		return "DONE"
	case "current":
		return "Working"
	case "failed":
		return "FAILED"
	case "skipped":
		return "Skipped"
	default:
		return "Waiting"
	}
}

func updateOperationLabel(kind string) string {
	if kind == update.KindCheck {
		return "Check for updates"
	}
	return "Download update"
}

func downloadOutcome(f *update.Failure, status update.Status) []string {
	var what string
	switch {
	case f.Phase == update.PhaseChecksum:
		what = "Nothing was downloaded."
	case f.Phase == update.PhaseDownload:
		what = "The partial download was discarded."
	case f.Class == update.ClassChecksumMismatch || f.Class == update.ClassAsset:
		what = "The downloaded file failed verification and was discarded."
	default:
		what = "The downloaded file was not put in place."
	}
	if status.Ready != nil && status.ReadyProblem == "" {
		return []string{what, "A previously verified update is still ready to install."}
	}
	return []string{what, "No update is ready to install."}
}

func updatePercent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	percent := float64(done) * 100 / float64(total)
	return math.Max(0, math.Min(100, percent))
}

func updateBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, suffix := range units {
		value /= unit
		if value < unit || suffix == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value)
}

func updateTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

// updatesPage renders only local update state. It never checks for releases.
func (h *handler) updatesPage(w http.ResponseWriter, r *http.Request) {
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	h.renderUpdates(w, "")
}

func (h *handler) renderUpdates(w http.ResponseWriter, message string) {
	data := updatesPageData{Locale: h.locale(), Token: h.token, Error: bounded(message, maxErrorMessage), Upd: h.updatesView()}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := updatesTemplate.Execute(w, data); err != nil {
		return
	}
}

func (h *handler) updatesChannel(w http.ResponseWriter, r *http.Request) {
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	channel, err := update.ParseChannel(strings.TrimSpace(r.PostFormValue("channel")))
	if err != nil {
		h.renderUpdateFailure(w, err, "Choose Stable or Development.")
		return
	}
	h.completeUpdateAction(w, r, h.updates.SetChannel(channel))
}

func (h *handler) updatesCheck(w http.ResponseWriter, r *http.Request) {
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	h.completeUpdateAction(w, r, h.updates.Check())
}

func (h *handler) updatesDownload(w http.ResponseWriter, r *http.Request) {
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	h.completeUpdateAction(w, r, h.updates.Download(strings.TrimSpace(r.PostFormValue("tag"))))
}

func (h *handler) updatesInstall(w http.ResponseWriter, r *http.Request) {
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	h.completeUpdateAction(w, r, h.updates.Install())
}

func (h *handler) completeUpdateAction(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		h.renderUpdateFailure(w, err, updateErrorHint(err))
		return
	}
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}

func (h *handler) renderUpdateFailure(w http.ResponseWriter, err error, hint string) {
	data := updatesPageData{Locale: h.locale(), Token: h.token, Error: bounded(err.Error(), maxErrorMessage), ErrorHint: hint, Upd: h.updatesView()}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := updatesTemplate.Execute(w, data); executeErr != nil {
		return
	}
}

func updateErrorHint(err error) string {
	var updateErr *update.Error
	if errors.As(err, &updateErr) {
		if hint := update.Hints[updateErr.Class]; hint != "" {
			return hint
		}
		if updateErr.Class == update.ClassRefused {
			return "This action is unavailable in the current update state. Review the selected channel, ready candidate and operation status before retrying."
		}
	}
	return "The update action could not be completed."
}
