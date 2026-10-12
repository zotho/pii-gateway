"""Minimal local replica of the presidio-analyzer docker image HTTP API (flask app.py).

Endpoints: GET /health, POST /analyze, GET /recognizers, GET /supportedentities.
Also answers GET /health as a stand-in for the image redactor (Palena readiness).
Config: presidio/{analyzer,nlp,recognizers}.yaml (Palena's configs + ru).
"""
import json
import logging
import os
from pathlib import Path

from flask import Flask, Response, jsonify, request
from presidio_analyzer import AnalyzerEngine, AnalyzerEngineProvider, RecognizerRegistry
from presidio_analyzer.analyzer_request import AnalyzerRequest

HERE = Path(__file__).parent
logging.basicConfig(level=logging.INFO)
log = logging.getLogger("presidio-app")

engine: AnalyzerEngine = AnalyzerEngineProvider(
    analyzer_engine_conf_file=HERE / "analyzer.yaml",
    nlp_engine_conf_file=HERE / "nlp.yaml",
    recognizer_registry_conf_file=HERE / "recognizers.yaml",
).create_engine()
log.info("engine ready, languages=%s", engine.supported_languages)

app = Flask(__name__)


@app.get("/health")
def health():
    return Response("Presidio Analyzer service is up", mimetype="text/plain")


@app.post("/analyze")
def analyze():
    try:
        body = request.get_json(force=True)
        req = AnalyzerRequest(body)
        if not req.text:
            raise ValueError("No text provided")
        if not req.language:
            raise ValueError("No language provided")
        results = engine.analyze(
            text=req.text,
            language=req.language,
            correlation_id=req.correlation_id,
            score_threshold=req.score_threshold,
            entities=req.entities,
            return_decision_process=req.return_decision_process,
            ad_hoc_recognizers=req.ad_hoc_recognizers,
            context=req.context,
            allow_list=req.allow_list,
            allow_list_match=req.allow_list_match,
            regex_flags=req.regex_flags,
        )
        return Response(
            json.dumps([r.to_dict() for r in results], ensure_ascii=False),
            content_type="application/json; charset=utf-8",
        )
    except Exception as e:  # noqa: BLE001
        log.exception("analyze failed")
        return jsonify(error=str(e)), 500


@app.get("/recognizers")
def recognizers():
    language = request.args.get("language", "en")
    return jsonify([r.name for r in engine.get_recognizers(language)])


@app.get("/supportedentities")
def supported_entities():
    language = request.args.get("language", "en")
    return jsonify(engine.get_supported_entities(language=language))


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "15002"))
    from waitress import serve
    serve(app, host="127.0.0.1", port=port, threads=4)
