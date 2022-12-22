package audioselection

type AudioSource interface {
	Activate()
	Deactivate()
	GetAudioLevel() uint8
}

type SelectionForworderParams struct {
}

type SelectionForworder struct {
}

func NewSelectionForworder() *SelectionForworder {
	return &SelectionForworder{}
}

func (f *SelectionForworder) AddDownTrack() {
}

func (f *SelectionForworder) RemoveDownTrack() {
}

func (f *SelectionForworder) OnRequestDowntrack() {
}
