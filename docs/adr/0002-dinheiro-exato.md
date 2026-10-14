# ADR 0002 — Dinheiro exato

## Decisão

Representar `Money` com `int64` em unidades mínimas e moeda, sem ponto flutuante.
Persistir como `BIGINT` e `TEXT`. Aceitar BRL, USD e EUR, com duas casas decimais.
Operações verificam overflow e compatibilidade de moedas, sem alterar os operandos.

Entradas externas são strings não negativas com duas casas e até 20 bytes,
sem arredondamento. Zeros à esquerda são normalizados. O zero de Go é inválido;
usar `Zero(currency)`. Valores negativos internos, como diferenças de reconciliação,
são reidratados por `FromMinorUnits`, não pelo decoder JSON externo.
A carteira exige saldo não negativo; o banco reforça essa regra.

## Motivo

Unidades mínimas evitam erros de precisão de ponto flutuante. Validar escala,
moeda e overflow impede arredondamentos silenciosos e cálculos incompatíveis.
A normalização faz valores equivalentes produzirem o mesmo hash de idempotência.
