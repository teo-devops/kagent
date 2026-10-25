import type { ScheduledRun } from "@/api/domain/scheduledRuns";
export interface ScheduledAgentRef { namespace: string; name: string; }
export function schedulesFor(schedules: readonly ScheduledRun[], agent: ScheduledAgentRef): ScheduledRun[] {
 return schedules.filter(schedule => schedule.metadata.namespace === agent.namespace && schedule.spec.agentRef.name === agent.name);
}
