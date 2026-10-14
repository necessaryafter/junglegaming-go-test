# ADR 0005 — Referências pendentes e reversões

## Decisão

Persistir `PENDING_REFERENCE` quando a referência não chegou ou ainda está pendente.
O worker retenta com backoff de 1 a 256 segundos e prazo de 24 horas. Ele bloqueia
carteira antes de transação, como o fluxo de entrada, e usa `SKIP LOCKED`.
Referência sem sucesso ou expirada resulta em rejeição definitiva.

Reversões são integrais e exigem o mesmo provedor, jogador, carteira, moeda e rodada.
`REFUND` e `ROLLBACK` disputam uma única reversão bem-sucedida da referência.
Desfazer um refund não libera a aposta original para nova devolução.
Estados terminais não mudam; reidratar não reaplica movimentos.

## Motivo

Persistir a espera permite receber mensagens fora de ordem e continuar após
um reinício. Limitar cada referência a uma reversão impede devoluções repetidas
por ciclos de refund e rollback. A rejeição persistida mantém o resultado de replays;
corrigir a operação exige outro ID externo e outra chave.
