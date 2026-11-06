package backup

import (
	"context"
	"errors"

	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

var errReadOnly = errors.New("this app can only look at and download the backup")

// readOnlyTG is a Telegram connection that can read but never change the
// channel.
type readOnlyTG struct{ telegram }

func (readOnlyTG) Upload(context.Context, tgclient.UploadRequest) (int, error) { return 0, errReadOnly }
func (readOnlyTG) DeleteMessages(context.Context, []int) error                 { return errReadOnly }
func (readOnlyTG) EditCaption(context.Context, int, string) error              { return errReadOnly }

// ReadOnly makes the engine unable to upload, delete or edit anything in
// Telegram, whatever it is asked to do. It is for the library app. Call it
// before Run.
func (e *Engine) ReadOnly() {
	e.tg = readOnlyTG{e.tg}
	e.readOnly = true
}
