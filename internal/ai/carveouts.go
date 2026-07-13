package ai

// heraldCarveouts are the feed-specific "this is NOT prompt injection" phrases
// appended to airlock's generic screening prompt via screen.Options.Exclusions.
//
// They are deliberately short. airlock's default prompt already excludes the
// broad categories Herald's old hand-rolled prompt listed -- political content,
// offensive or alarming subject matter, misinformation, marketing and affiliate
// links, and attack code QUOTED as an example in a write-up. Repeating those
// here would just be the drift plan 012 exists to end. What remains is the one
// genuinely feed-specific case: alternative front-ends and mirrors, which a
// small model (Gemma) otherwise tends to treat as suspicious.
//
// Add to this list only a false positive that airlock's generic prompt does not
// already cover. If the model keeps flagging something that is plainly ordinary
// feed content, a short phrase here is the tuning surface -- not a fork of the
// prompt.
var heraldCarveouts = []string{
	"Links to alternative front-ends or mirrors such as nitter, xcancel, or teddit",
	"News references to diseases, pathogens, biosecurity incidents, weapons, or other hazardous topics -- naming or reporting a danger is subject matter, not an instruction to an AI",
}
