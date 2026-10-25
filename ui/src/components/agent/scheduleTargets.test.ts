import { describe, expect, it } from "vitest";
import type { ScheduledRun } from "@/api/domain/scheduledRuns";
import { schedulesFor } from "./scheduleTargets";

describe("schedulesFor", () => {
  it("matches only the exact Agent namespace and name", () => {
    const mine = { metadata: { name: "mine", namespace: "team" }, spec: { agentRef: { name: "assistant" }, prompt: "Report", schedule: "* * * * *" } } satisfies ScheduledRun;
    const otherAgent = { metadata: { name: "other", namespace: "team" }, spec: { agentRef: { name: "other" }, prompt: "Report", schedule: "* * * * *" } } satisfies ScheduledRun;
    const otherNamespace = { metadata: { name: "elsewhere", namespace: "elsewhere" }, spec: { agentRef: { name: "assistant" }, prompt: "Report", schedule: "* * * * *" } } satisfies ScheduledRun;
    const absent: ScheduledRun = { metadata: { name: "absent" }, spec: { agentRef: { name: "report" }, prompt: "Report", schedule: "* * * * *" } };
    expect(schedulesFor([mine,otherAgent,otherNamespace,absent], {namespace:"team",name:"assistant"})).toEqual([mine]);
  });
});
