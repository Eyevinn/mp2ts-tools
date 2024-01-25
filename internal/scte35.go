package internal

import (
	"github.com/Comcast/gots/v2/scte35"
)

type SCTE35Info struct {
	PID           uint16                   `json:"pid"`
	SpliceCommand SpliceCommand            `json:"spliceCommand"`
	SegDesc       []SegmentationDescriptor `json:"segmentationDes,omitempty"`
}

type SpliceCommand struct {
	Type      string `json:"type"`
	EventId   uint32 `json:"eventId"`
	PTS       uint64 `json:"pts"`
	Duration  uint64 `json:"duration,omitempty"`
	Out       bool   `json:"outOfNetwork,omitempty"`
	Immediate bool   `json:"immediate,omitempty"`
}

type SegmentationDescriptor struct {
	SegmentNumber uint8  `json:"segmentNumber"`
	EventId       uint32 `json:"eventId"`
	Type          string `json:"type"`
	Duration      uint64 `json:"duration,omitempty"`
}

func toSCTE35(pid uint16, msg scte35.SCTE35) SCTE35Info {
	scte35Info := SCTE35Info{PID: pid, SpliceCommand: toSpliceCommand(msg.CommandInfo())}

	if insert, ok := msg.CommandInfo().(scte35.SpliceInsertCommand); ok {
		scte35Info.SpliceCommand = toSpliceInsertCommand(insert)
	}
	for _, desc := range msg.Descriptors() {
		segDesc := toSegmentationDescriptor(desc)
		scte35Info.SegDesc = append(scte35Info.SegDesc, segDesc)
	}

	return scte35Info
}

func toSpliceCommand(spliceCommand scte35.SpliceCommand) SpliceCommand {
	spliceCmd := SpliceCommand{Type: getCommandType(spliceCommand)}
	if spliceCommand.HasPTS() {
		spliceCmd.PTS = uint64(spliceCommand.PTS())
	}

	return spliceCmd
}

func toSegmentationDescriptor(segdesc scte35.SegmentationDescriptor) SegmentationDescriptor {
	segDesc := SegmentationDescriptor{}
	segDesc.EventId = segdesc.EventID()
	segDesc.Type = scte35.SegDescTypeNames[segdesc.TypeID()]
	segDesc.SegmentNumber = segdesc.SegmentNumber()
	if segdesc.HasDuration() {
		segDesc.Duration = uint64(segdesc.Duration())
	}
	return segDesc
}

func toSpliceInsertCommand(spliceCommand scte35.SpliceInsertCommand) SpliceCommand {
	spliceCmd := SpliceCommand{Type: getCommandType(spliceCommand)}
	spliceCmd.EventId = spliceCommand.EventID()
	spliceCmd.Immediate = spliceCommand.SpliceImmediate()
	spliceCmd.Out = spliceCommand.IsOut()
	if spliceCommand.HasPTS() {
		spliceCmd.PTS = uint64(spliceCommand.PTS())
	}
	if spliceCommand.HasDuration() {
		spliceCmd.Duration = uint64(spliceCommand.Duration())
	}

	return spliceCmd
}

func getCommandType(spliceCommand scte35.SpliceCommand) string {
	return scte35.SpliceCommandTypeNames[spliceCommand.CommandType()]
}
