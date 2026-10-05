import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { appendFileSync } from "node:fs";

export default function (pi: ExtensionAPI) {
	const path = process.env.NM_PROOF_PREFIX + ".provider.jsonl";
	function record(value: unknown) {
		appendFileSync(path, JSON.stringify(value) + "\n", { mode: 0o600 });
	}
	pi.on("before_provider_request", (event, ctx) => {
		const payload = event.payload as any;
		const outputTool = payload.tools?.find((tool: any) =>
			(tool.name ?? tool.function?.name) === "no_mistakes_output");
		record({ type: "request", model: ctx.model?.id, provider: ctx.model?.provider,
			payloadModel: payload.model, toolChoice: payload.tool_choice,
			outputTool: outputTool ? { name: outputTool.name ?? outputTool.function?.name,
				strict: outputTool.strict ?? outputTool.function?.strict,
				parameters: outputTool.parameters ?? outputTool.function?.parameters } : null });
	});
	pi.on("after_provider_response", (event) => {
		record({ type: "http_response", status: event.status });
	});
	const seen = new Set<string>();
	pi.on("provider_stream_event", (event) => {
		const data = event.data as any;
		const responseModel = data.response?.model ?? data.model;
		if (typeof responseModel !== "string") return;
		const key = `${event.provider}/${event.model}/${responseModel}`;
		if (seen.has(key)) return;
		seen.add(key);
		record({ type: "provider_model", provider: event.provider, model: event.model,
			api: event.api, responseModel, eventType: data.type });
	});
}
