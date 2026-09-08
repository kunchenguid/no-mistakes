package config

// PublishesIntent controls only the public Intent section, never the input
// available to review or PR drafting. It makes no promise about model prose.
func (p PR) PublishesIntent() bool {
	return p.PublishIntent == nil || *p.PublishIntent
}
