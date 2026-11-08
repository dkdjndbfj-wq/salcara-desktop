package protocol

const MaxFeatureSkills = 10000
const MaxFeatureCatalogBytes = 16 << 20

// FeatureCatalogPage is an opt-in v1 directory transfer. Revision covers public
// display metadata only, not skill bodies. Skills are upserts for a delta.
// A client publishes nothing until all pages of this immutable transfer arrive.
type FeatureCatalogPage struct {
	SyncVersion        int              `json:"syncVersion"`
	Kind               string           `json:"kind"` // full | delta
	Revision           string           `json:"revision"`
	BaseRevision       string           `json:"baseRevision,omitempty"`
	CollaborationModes []string         `json:"collaborationModes"`
	SkillsSupported    bool             `json:"skillsSupported"`
	Skills             []Skill          `json:"skills"`
	RemovedSkills      []SkillReference `json:"removedSkills"`
	TotalSkills        int              `json:"totalSkills"`
	TotalChanges       int              `json:"totalChanges"`
	Offset             int              `json:"offset"`
	NextCursor         string           `json:"nextCursor,omitempty"`
}
