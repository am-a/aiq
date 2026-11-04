package asks

const messagePreamble = "The state is the message a coding agent wrote when it stopped working, read by the person running the session. "

var ruleQuestions = map[string]Question{
	"asks_reader": {
		Instructions: messagePreamble + "Does the message ask the reader to personally answer, decide, approve, test, send or run something?",
		True:         "The message says what the reader must do or choose, such as 'your call', 'waiting on you', 'please merge', 'run this', or gives the reader a draft to send.",
		False:        "The message reports what the agent or other agents did or are doing. Things waiting on CI, a named agent or colleague do not count.",
	},
	"risk_for_reader": {
		Instructions: messagePreamble + "Does the message warn the reader about a security exposure, data loss or a production problem that a person must deal with?",
		True:         "The message warns of a security exposure, data loss or production problem requiring the reader's action.",
		False:        "The message reports normal work or a problem that does not require the reader to deal with it.",
	},
	"holds_item": {
		Instructions: "The state is one paragraph from a coding agent's message to the person running the session. Does this paragraph ask that person to answer, decide, approve or do something, warn them of a risk, or announce a default the agent will take unless told otherwise?",
		True:         "The paragraph asks the reader for an answer, decision or action, warns of a risk, or announces a default unless told otherwise.",
		False:        "The paragraph only reports work or something waiting on someone else.",
	},
	"settles": {
		Instructions: "The state holds an ITEM a coding agent raised for the person, and the person's later REPLY. Does the REPLY settle the whole ITEM, so that nothing in it is still waiting on the person?",
		True:         "The reply settles the whole item and leaves nothing waiting on the person.",
		False:        "The reply leaves any part of the item unanswered or waiting on the person.",
	},
	"same": {
		Instructions: "The state holds two items a coding agent raised for the same person, A and B. Do A and B ask the person for the same decision or action?",
		True:         "Both items ask the person for the same decision or action.",
		False:        "The items ask for different decisions or actions, even if they mention the same subject.",
	},
}
