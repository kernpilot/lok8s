"""Configurable QLoRA trainer. Backends: unsloth | peft.

Trains the format adapter on the CONDUCTOR's base model so it can be toggled on
the resident model at runtime (llama.cpp /lora-adapters) — one model, no swap.

Heavy imports (torch/transformers/peft/unsloth) are done lazily so the benchmark
harness never needs them. Run on the gfx1201 box with requirements-train.txt.
"""
from __future__ import annotations

import json

SYSTEM = ("You are the lok8s YAML author. Given a request, output ONLY the "
          "complete, valid lok8s YAML — no prose, no code fences.")


def _load_pairs(path):
    rows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    if not rows:
        raise SystemExit(f"no training pairs in {path} — run `synth` then `verify` first")
    return rows


def _messages(intent, yaml_text):
    return [{"role": "system", "content": SYSTEM},
            {"role": "user", "content": intent},
            {"role": "assistant", "content": yaml_text}]


def train(cfg) -> None:
    q = cfg["train"]["qlora"]
    pairs_path = cfg.resolve(cfg.get("train.verify.out", "./data/pairs.verified.jsonl"))
    rows = _load_pairs(pairs_path)
    out_dir = str(cfg.resolve(q["out_dir"]))
    backend = q.get("backend", "unsloth")
    print(f"training {backend} QLoRA on {len(rows)} pairs -> {out_dir}")

    if backend == "unsloth":
        _train_unsloth(q, rows, out_dir)
    elif backend == "peft":
        _train_peft(q, rows, out_dir)
    else:
        raise SystemExit(f"unknown train backend: {backend}")


def _dataset(rows, tokenizer):
    from datasets import Dataset
    texts = [tokenizer.apply_chat_template(_messages(r["intent"], r["yaml"]),
                                           tokenize=False) for r in rows]
    return Dataset.from_dict({"text": texts})


def _sft_config(q):
    from trl import SFTConfig
    return SFTConfig(
        output_dir=q["out_dir"],
        per_device_train_batch_size=int(q.get("batch_size", 1)),
        gradient_accumulation_steps=int(q.get("grad_accum", 4)),
        num_train_epochs=float(q.get("epochs", 3)),
        learning_rate=float(q.get("learning_rate", 2e-4)),
        max_length=int(q.get("max_seq_len", 4096)),
        logging_steps=5,
        optim="adamw_8bit",
        gradient_checkpointing=True,
        dataset_text_field="text",
        report_to="none",
    )


def _train_unsloth(q, rows, out_dir):
    from unsloth import FastLanguageModel
    from trl import SFTTrainer

    model, tokenizer = FastLanguageModel.from_pretrained(
        model_name=q["base_model"],
        max_seq_length=int(q.get("max_seq_len", 4096)),
        load_in_4bit=bool(q.get("load_in_4bit", True)),
    )
    model = FastLanguageModel.get_peft_model(
        model,
        r=int(q.get("rank", 16)),
        lora_alpha=int(q.get("lora_alpha", 32)),
        lora_dropout=float(q.get("lora_dropout", 0.0)),
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj",
                        "gate_proj", "up_proj", "down_proj"],
        use_gradient_checkpointing="unsloth",
    )
    trainer = SFTTrainer(model=model, tokenizer=tokenizer,
                         train_dataset=_dataset(rows, tokenizer),
                         args=_sft_config(q))
    trainer.train()
    model.save_pretrained(out_dir)
    tokenizer.save_pretrained(out_dir)
    if q.get("export_gguf"):
        try:
            model.save_pretrained_gguf(out_dir, tokenizer)
        except Exception as e:
            print(f"gguf export skipped ({e}); adapter saved to {out_dir}")
    print(f"adapter -> {out_dir}")


def _train_peft(q, rows, out_dir):
    import torch
    from transformers import (AutoModelForCausalLM, AutoTokenizer,
                              BitsAndBytesConfig)
    from peft import LoraConfig, get_peft_model, prepare_model_for_kbit_training
    from trl import SFTTrainer

    tokenizer = AutoTokenizer.from_pretrained(q["base_model"])
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token
    bnb = BitsAndBytesConfig(
        load_in_4bit=bool(q.get("load_in_4bit", True)),
        bnb_4bit_quant_type="nf4",
        bnb_4bit_compute_dtype=torch.bfloat16,
        bnb_4bit_use_double_quant=True,
    )
    model = AutoModelForCausalLM.from_pretrained(
        q["base_model"], quantization_config=bnb, device_map="auto")
    model = prepare_model_for_kbit_training(model)
    model = get_peft_model(model, LoraConfig(
        r=int(q.get("rank", 16)), lora_alpha=int(q.get("lora_alpha", 32)),
        lora_dropout=float(q.get("lora_dropout", 0.0)), bias="none",
        task_type="CAUSAL_LM",
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj",
                        "gate_proj", "up_proj", "down_proj"]))
    trainer = SFTTrainer(model=model, tokenizer=tokenizer,
                         train_dataset=_dataset(rows, tokenizer),
                         args=_sft_config(q))
    trainer.train()
    model.save_pretrained(out_dir)
    tokenizer.save_pretrained(out_dir)
    print(f"adapter -> {out_dir} (convert to gguf with llama.cpp "
          "convert_lora_to_gguf.py for runtime toggle)")
